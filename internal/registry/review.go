package registry

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

type adminKey struct{}

func WithAdmin(ctx context.Context) context.Context { return context.WithValue(ctx, adminKey{}, true) }

func isAdmin(ctx context.Context) bool { v, _ := ctx.Value(adminKey{}).(bool); return v }

func (s *Service) reviewRule(ctx context.Context, indexed bool) model.ReviewRule {
	return model.ReviewRule{Enabled: s.Cfg.Review && !isAdmin(ctx), Always: indexed}
}

var handleRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,37}[a-z0-9])$`)

var reservedHandles = []string{"admin", "administrator", "openlinker", "official", "registry", "support", "security",
	"root", "system", "staff", "team", "help", "api", "www", "mcp", "skills"}

const (
	openLinkerPrefix = "ai.openlinker."
	githubPrefix     = "io.github."
)

type Claimant struct {
	Actor      string
	Subject    string
	GitHubID   string
	OpenLinker bool
	GitHub     bool
}

func notClaimable(msg string) error {
	return fail(http.StatusUnprocessableEntity, "namespace_not_claimable", msg)
}

func (s *Service) ClaimNamespace(ctx context.Context, c Claimant, name string) (model.Namespace, bool, error) {
	switch {
	case strings.HasPrefix(name, openLinkerPrefix):
		handle := strings.TrimPrefix(name, openLinkerPrefix)
		if s.Cfg.Mode != "public" {
			return model.Namespace{}, false, notClaimable("ai.openlinker namespaces exist only on the public instance")
		}
		if !handleRe.MatchString(handle) || strings.Contains(handle, "--") || slices.Contains(reservedHandles, handle) {
			return model.Namespace{}, false, notClaimable("the handle must be 3-39 lowercase letters, digits or single hyphens, not reserved")
		}
		if !c.OpenLinker {
			return model.Namespace{}, false, fail(http.StatusForbidden, "identity_not_allowed", "this gateway cannot vouch for OpenLinker accounts")
		}
		return s.Store.ClaimNamespace(ctx, model.Namespace{Name: name, Kind: "openlinker", OpenLinkerSubject: c.Subject}, c.Subject,
			auditEvent(c.Actor, "namespace.claim", name, "", map[string]any{"kind": "openlinker", "subject": c.Subject}))
	case strings.HasPrefix(name, githubPrefix):
		login := strings.TrimPrefix(name, githubPrefix)
		if !c.GitHub {
			return model.Namespace{}, false, fail(http.StatusForbidden, "identity_not_allowed", "this gateway cannot vouch for GitHub accounts")
		}
		if c.GitHubID == "" {
			return model.Namespace{}, false, fail(http.StatusForbidden, "github_identity_required", "sign in with GitHub to claim an io.github namespace")
		}
		id, err := strconv.ParseInt(c.GitHubID, 10, 64)
		if err != nil || id <= 0 {
			return model.Namespace{}, false, fail(http.StatusForbidden, "github_identity_required", "sign in with GitHub to claim an io.github namespace")
		}
		acct, err := s.githubAccount(ctx, id)
		if err != nil {
			return model.Namespace{}, false, err
		}
		if acct.Type != "User" {
			return model.Namespace{}, false, fail(http.StatusForbidden, "github_org_unsupported", "organization namespaces cannot be claimed through a gateway")
		}
		if strings.ToLower(acct.Login) != login {
			return model.Namespace{}, false, fail(http.StatusForbidden, "github_login_mismatch", "the namespace must be io.github.<your GitHub login>")
		}
		return s.Store.ClaimNamespace(ctx, model.Namespace{Name: name, Kind: "github", GitHubID: &id, DisplayName: acct.Login}, "",
			auditEvent(c.Actor, "namespace.claim", name, "", map[string]any{"kind": "github", "github_id": id}))
	}
	return model.Namespace{}, false, notClaimable("only ai.openlinker.<handle> and io.github.<login> can be claimed")
}

type githubAccount struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (s *Service) githubAccount(ctx context.Context, id int64) (githubAccount, error) {
	var acct githubAccount
	if err := s.GitHub.API(ctx, "/user/"+strconv.FormatInt(id, 10), &acct); err != nil {
		return acct, err
	}
	if acct.ID != id || acct.Login == "" {
		return acct, fail(http.StatusBadGateway, "github_unavailable", "GitHub returned an unexpected account")
	}
	return acct, nil
}

func (s *Service) githubRepoID(ctx context.Context, owner, repo string) (int64, error) {
	var info struct {
		ID int64 `json:"id"`
	}
	if err := s.GitHub.API(ctx, "/repos/"+owner+"/"+repo, &info); err != nil || info.ID <= 0 {
		return 0, fail(http.StatusBadGateway, "github_unavailable", "the repository id could not be confirmed on GitHub")
	}
	return info.ID, nil
}

func (s *Service) ClaimSkill(ctx context.Context, c Claimant, sk model.Skill, newName string) (model.Skill, error) {
	if sk.Mode != "indexed" {
		return sk, fail(http.StatusUnprocessableEntity, "claim_not_indexed", "only indexed entries can be claimed")
	}
	if sk.Upstream != "" {
		return sk, model.ErrNamespaceManaged()
	}
	if newName != "" && !skill.ValidSkillName(newName) {
		return sk, fail(http.StatusUnprocessableEntity, "name_invalid", "skill name must be a lowercase slug (a-z, 0-9, single hyphens, max 64)")
	}
	vs, err := s.Store.ListVersionSummaries(ctx, sk.ID)
	if err != nil {
		return sk, err
	}
	var repoID int64
	github := false
	for _, v := range vs {
		if v.Locator == nil || v.Locator.Kind != "github" {
			continue
		}
		github = true
		if v.Locator.RepoID <= 0 || (repoID != 0 && v.Locator.RepoID != repoID) {
			return sk, fail(http.StatusForbidden, "claim_unverifiable", "this entry has no verifiable GitHub repository id")
		}
		repoID = v.Locator.RepoID
	}
	if !github {
		return sk, fail(http.StatusUnprocessableEntity, "claim_not_github", "only entries indexed from GitHub can be claimed")
	}
	if repoID == 0 {
		return sk, fail(http.StatusForbidden, "claim_unverifiable", "this entry has no verifiable GitHub repository id")
	}
	if !c.GitHub || c.GitHubID == "" {
		return sk, fail(http.StatusForbidden, "github_identity_required", "sign in with GitHub to claim an entry")
	}
	var repoInfo struct {
		ID    int64         `json:"id"`
		Owner githubAccount `json:"owner"`
	}
	if err := s.GitHub.API(ctx, "/repositories/"+strconv.FormatInt(repoID, 10), &repoInfo); err != nil {
		return sk, fail(http.StatusBadGateway, "github_unavailable", "the source repository could not be read from GitHub")
	}
	if repoInfo.ID != repoID {
		return sk, fail(http.StatusBadGateway, "github_unavailable", "GitHub returned a different repository")
	}
	if repoInfo.Owner.Type != "User" {
		return sk, fail(http.StatusForbidden, "github_org_unsupported", "entries from organization repositories cannot be claimed through a gateway")
	}
	if strconv.FormatInt(repoInfo.Owner.ID, 10) != c.GitHubID {
		return sk, fail(http.StatusForbidden, "claim_not_owner", "only the owner of the source repository can claim this entry")
	}
	ns, err := s.Store.NamespaceForGitHubID(ctx, repoInfo.Owner.ID)
	if errors.Is(err, model.ErrNotFound) {
		ns, _, err = s.ClaimNamespace(ctx, c, githubPrefix+strings.ToLower(repoInfo.Owner.Login))
	}
	if err != nil {
		return sk, err
	}
	if sk.Namespace == ns.Name && (newName == "" || newName == sk.Name) {
		return sk, nil
	}
	if newName == "" {
		newName = sk.Name
	}
	return s.Store.MoveSkill(model.WithSkillGuard(ctx, sk.ID, sk.Namespace), sk.ID, ns.Name, newName, sk.ReviewRevision,
		auditEvent(c.Actor, "skill.claim", ns.Name, newName, map[string]any{"from_namespace": sk.Namespace, "from_name": sk.Name,
			"indexed_by": vs[len(vs)-1].IndexedBy, "github_id": c.GitHubID, "repo_id": repoID}))
}

var reportCategories = []string{"malware", "credential", "license", "spam", "impersonation", "other"}

func (s *Service) Report(ctx context.Context, actor string, rep model.Report, admin bool) (model.ReportResult, error) {
	if !slices.Contains(reportCategories, rep.Category) {
		return model.ReportResult{}, fail(http.StatusUnprocessableEntity, "category_invalid", "category must be one of "+strings.Join(reportCategories, ", "))
	}
	if len(rep.Details) > 2000 || len(rep.Version) > 255 {
		return model.ReportResult{}, fail(http.StatusUnprocessableEntity, "report_too_long", "details are limited to 2000 bytes")
	}
	threshold := s.Cfg.ReportThreshold
	if threshold <= 0 {
		threshold = 3
	}
	return s.Store.CreateReport(ctx, rep, threshold, admin, auditEvent(actor, "report.create", rep.Namespace, rep.Name,
		map[string]any{"kind": rep.Kind, "category": rep.Category}))
}

func (s *Service) ResolveReport(ctx context.Context, actor string, id int64, resolution string) (model.Report, error) {
	if resolution != "dismissed" && resolution != "actioned" {
		return model.Report{}, fail(http.StatusUnprocessableEntity, "resolution_invalid", "resolution must be dismissed or actioned")
	}
	return s.Store.ResolveReport(ctx, id, resolution, actor, s.Now(), auditEvent(actor, "report.resolve", "", strconv.FormatInt(id, 10),
		map[string]any{"resolution": resolution}))
}

func (s *Service) DecideReview(ctx context.Context, actor, kind string, id, revision int64, ns, name, decision, reason string, trust bool) error {
	state := ""
	switch decision {
	case "approve":
		state = model.ReviewApproved
	case "reject":
		state = model.ReviewRejected
	default:
		return fail(http.StatusUnprocessableEntity, "decision_invalid", "decision must be approve or reject")
	}
	if len(reason) > 500 {
		return fail(http.StatusUnprocessableEntity, "reason_too_long", "reason is limited to 500 bytes")
	}
	return s.Store.DecideReview(ctx, model.ReviewDecision{Kind: kind, ID: id, State: state, Reason: reason, Revision: revision, TrustNamespace: trust},
		auditEvent(actor, "review."+decision, ns, name, map[string]any{"kind": kind, "trust_namespace": trust}))
}
