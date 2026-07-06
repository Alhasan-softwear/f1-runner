package provision

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string][2]string{
		"python":   {"python", ""},
		"python3":  {"python", ""},
		"mysql":    {"mariadb", ""},
		"node@22":  {"node", "22"},
		"NodeJS":   {"node", ""},
		"postgres": {"postgres", ""},
		"apache":   {"apache", ""},
		"php":      {"php", ""},
	}
	for in, want := range cases {
		name, ver, err := Normalize(in)
		if err != nil {
			t.Errorf("Normalize(%q): %v", in, err)
			continue
		}
		if name != want[0] || ver != want[1] {
			t.Errorf("Normalize(%q) = %s@%s, want %s@%s", in, name, ver, want[0], want[1])
		}
	}
	if _, _, err := Normalize("laravel"); err == nil {
		t.Error("unknown package should error")
	}
}

func TestRecipesCoverAllManagers(t *testing.T) {
	for name, r := range recipes {
		if r.check == "" {
			t.Errorf("%s: missing check", name)
		}
		for _, mgr := range []string{"apt", "apk", "dnf"} {
			if _, ok := r.pkgs[mgr]; !ok && len(r.pre[mgr]) == 0 {
				t.Errorf("%s: no install path for %s", name, mgr)
			}
		}
	}
}
