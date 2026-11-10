package config

import "testing"

func TestLoad(t *testing.T) {
	env := map[string]string{"REGISTRY_DATABASE_URL": "postgres://x", "REGISTRY_PUBLIC_URL": "https://r.example/registry/", "REGISTRY_MODE": "public"}
	get := func(k string) string { return env[k] }
	c, err := Load([]string{"-base-path", "/registry"}, get)
	if err != nil || c.PublicURL != "https://r.example/registry" || !c.AllowAnonRead || c.BasePath != "/registry" {
		t.Fatalf("%+v %v", c, err)
	}
	env["REGISTRY_MODE"] = "private"
	if c, _ := Load(nil, get); c.AllowAnonRead {
		t.Fatal("private mode must default to authenticated reads")
	}
	for _, bad := range [][]string{{"-base-path", "registry"}, {"-base-path", "/r/"}, {"-public-url", "https://u:p@x"}, {"-public-url", "https://x?sig=1"}, {"-mode", "x"}, {"-max-files", "0"}} {
		if _, err := Load(bad, get); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	// Base path and public URL must agree.
	env["REGISTRY_PUBLIC_URL"] = "https://r.example"
	if _, err := Load([]string{"-base-path", "/registry"}, get); err == nil {
		t.Fatal("public URL without the base path accepted")
	}
	if _, err := Load([]string{"-base-path", "/registry", "-public-url", "https://r.example/edge/registry"}, get); err != nil {
		t.Fatalf("proxy prefix before base path rejected: %v", err)
	}
	if _, err := Load(nil, get); err != nil {
		t.Fatalf("no base path: %v", err)
	}
	if _, err := Load([]string{"-verify-interval", "-1s"}, get); err == nil {
		t.Fatal("negative verify interval accepted")
	}
	env["REGISTRY_PUBLIC_URL"] = "https://r.example/registry/"
	env["REGISTRY_ADMIN_TOKEN"] = "short"
	if _, err := Load(nil, get); err == nil {
		t.Fatal("short admin token accepted")
	}
}
