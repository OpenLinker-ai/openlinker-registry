// Package config loads server configuration from environment variables,
// overridable by command-line flags.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

// Config is the server configuration.
type Config struct {
	Listen        string
	BasePath      string
	DatabaseURL   string
	StorageDir    string
	PublicURL     string
	Mode          string // public | private
	AdminToken    string
	AllowAnonRead bool
	AllowPublic   bool
	AutoMigrate   bool

	GitHubToken   string
	GitHubAPIURL  string
	GitHubRawURL  string
	GitHubWebHost string
	GitHubTimeout time.Duration

	MaxUploadBytes int64
	Limits         skill.Limits
	CacheMaxAge    time.Duration
	CacheMaxBytes  int64
	// VerifyInterval bounds how long cached indexed content is served
	// before the source is re-verified.
	VerifyInterval time.Duration
	// ArtifactGCGrace is how old an unreferenced artifact must be before it
	// is garbage collected.
	ArtifactGCGrace time.Duration
}

// Load reads REGISTRY_* environment variables and then args.
func Load(args []string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	env := func(k, def string) string {
		if v := getenv("REGISTRY_" + k); v != "" {
			return v
		}
		return def
	}
	c := Config{}
	fs := flag.NewFlagSet("openlinker-registry", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", env("LISTEN", ":8080"), "listen address")
	fs.StringVar(&c.BasePath, "base-path", env("BASE_PATH", ""), `URL path prefix, e.g. "/registry" or ""`)
	fs.StringVar(&c.DatabaseURL, "database-url", env("DATABASE_URL", ""), "PostgreSQL DSN (prefer the environment variable)")
	fs.StringVar(&c.StorageDir, "storage-dir", env("STORAGE_DIR", "./data"), "local blob storage directory")
	fs.StringVar(&c.PublicURL, "public-url", env("PUBLIC_URL", ""), "external URL of the registry root, including any base path")
	fs.StringVar(&c.Mode, "mode", env("MODE", "private"), "instance mode: public or private")
	fs.StringVar(&c.GitHubAPIURL, "github-api-url", env("GITHUB_API_URL", "https://api.github.com"), "GitHub API base URL")
	fs.StringVar(&c.GitHubRawURL, "github-raw-url", env("GITHUB_RAW_URL", "https://raw.githubusercontent.com"), "GitHub raw content base URL")
	fs.StringVar(&c.GitHubWebHost, "github-web-host", env("GITHUB_WEB_HOST", "github.com"), "host accepted in GitHub source URLs")
	anon := fs.String("allow-anonymous-read", env("ALLOW_ANONYMOUS_READ", ""), "allow anonymous reads (default: true in public mode, false in private mode)")
	pub := fs.String("allow-public", env("ALLOW_PUBLIC_VISIBILITY", "true"), "allow public visibility")
	mig := fs.String("auto-migrate", env("AUTO_MIGRATE", "true"), "apply database migrations at startup")
	ghTimeout := fs.String("github-timeout", env("GITHUB_TIMEOUT", "20s"), "timeout for each GitHub request")
	maxUpload := fs.String("max-upload-bytes", env("MAX_UPLOAD_BYTES", strconv.Itoa(16<<20)), "maximum upload request body")
	maxFiles := fs.String("max-files", env("MAX_FILES", strconv.Itoa(skill.DefaultLimits.MaxFiles)), "maximum files per skill")
	maxFile := fs.String("max-file-bytes", env("MAX_FILE_BYTES", strconv.FormatInt(skill.DefaultLimits.MaxFileBytes, 10)), "maximum bytes per file")
	maxTotal := fs.String("max-total-bytes", env("MAX_TOTAL_BYTES", strconv.FormatInt(skill.DefaultLimits.MaxTotalBytes, 10)), "maximum bytes per skill")
	cacheAge := fs.String("cache-max-age", env("CACHE_MAX_AGE", "720h"), "evict cached indexed content older than this")
	verify := fs.String("verify-interval", env("VERIFY_INTERVAL", "1h"), "re-verify indexed sources after this long (0 = every read)")
	gcGrace := fs.String("artifact-gc-grace", env("ARTIFACT_GC_GRACE", "24h"), "minimum age of unreferenced artifacts before deletion")
	cacheBytes := fs.String("cache-max-bytes", env("CACHE_MAX_BYTES", strconv.Itoa(1<<30)), "cache capacity in bytes")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	// Secrets are read only from the environment so they never appear in
	// process listings.
	c.AdminToken = getenv("REGISTRY_ADMIN_TOKEN")
	c.GitHubToken = getenv("REGISTRY_GITHUB_TOKEN")

	var err error
	parseBool := func(name, v string, def bool) bool {
		if v == "" {
			return def
		}
		b, perr := strconv.ParseBool(v)
		if perr != nil && err == nil {
			err = fmt.Errorf("%s: invalid boolean", name)
		}
		return b
	}
	parseInt := func(name, v string) int64 {
		n, perr := strconv.ParseInt(v, 10, 64)
		if (perr != nil || n <= 0) && err == nil {
			err = fmt.Errorf("%s: must be a positive integer", name)
		}
		return n
	}
	parseDur := func(name, v string) time.Duration {
		d, perr := time.ParseDuration(v)
		if (perr != nil || d <= 0) && err == nil {
			err = fmt.Errorf("%s: invalid duration", name)
		}
		return d
	}
	c.AllowAnonRead = parseBool("allow-anonymous-read", *anon, c.Mode == "public")
	c.AllowPublic = parseBool("allow-public", *pub, true)
	c.AutoMigrate = parseBool("auto-migrate", *mig, true)
	c.GitHubTimeout = parseDur("github-timeout", *ghTimeout)
	c.MaxUploadBytes = parseInt("max-upload-bytes", *maxUpload)
	c.Limits = skill.Limits{MaxFiles: int(parseInt("max-files", *maxFiles)), MaxFileBytes: parseInt("max-file-bytes", *maxFile),
		MaxTotalBytes: parseInt("max-total-bytes", *maxTotal)}
	c.CacheMaxAge = parseDur("cache-max-age", *cacheAge)
	c.CacheMaxBytes = parseInt("cache-max-bytes", *cacheBytes)
	if v, perr := time.ParseDuration(*verify); perr != nil || v < 0 {
		if err == nil {
			err = errors.New("verify-interval: invalid duration")
		}
	} else {
		c.VerifyInterval = v
	}
	c.ArtifactGCGrace = parseDur("artifact-gc-grace", *gcGrace)
	if err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}

// Validate checks consistency.
func (c *Config) Validate() error {
	if c.Mode != "public" && c.Mode != "private" {
		return errors.New("mode must be public or private")
	}
	if c.BasePath != "" {
		if !strings.HasPrefix(c.BasePath, "/") || strings.HasSuffix(c.BasePath, "/") || strings.ContainsAny(c.BasePath, "?#%* ") {
			return errors.New(`base path must be "" or start with "/" and not end with "/"`)
		}
	}
	if c.DatabaseURL == "" {
		return errors.New("REGISTRY_DATABASE_URL is required")
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("REGISTRY_PUBLIC_URL must be an absolute http(s) URL without credentials, query or fragment")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	// With a base path the server only answers under that prefix, so the
	// public URL must end with it (a proxy may add its own prefix in front).
	// Without a base path any public path is allowed (proxy strips it).
	if c.BasePath != "" && !strings.HasSuffix(strings.TrimRight(u.Path, "/"), c.BasePath) {
		return errors.New("REGISTRY_PUBLIC_URL path must end with REGISTRY_BASE_PATH (e.g. https://host" + c.BasePath + ")")
	}
	if c.AdminToken != "" && len(c.AdminToken) < 32 {
		return errors.New("REGISTRY_ADMIN_TOKEN must be at least 32 characters")
	}
	if c.Limits.MaxFileBytes > c.Limits.MaxTotalBytes {
		return errors.New("max-file-bytes must not exceed max-total-bytes")
	}
	return nil
}
