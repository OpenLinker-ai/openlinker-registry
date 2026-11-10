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
	env["REGISTRY_ADMIN_TOKEN"] = "short"
	if _, err := Load(nil, get); err == nil {
		t.Fatal("short admin token accepted")
	}
}
