package imagefile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageVersionParsing(t *testing.T) {
	for _, schema := range []int{2} {
		for _, value := range []string{"0.1.0", "1.2.3-rc.1+build.7", "999999999999999999999.0.0"} {
			dir := writeV2Source(t, fmt.Sprintf("schema_version: %d\nimage_version: %s\n", schema, value))
			if _, err := ParseV2(dir); err != nil {
				t.Errorf("%d %s: %v", schema, value, err)
			}
		}
		for _, value := range []string{"v1.2.3", "1.2", "01.2.3", "1.2.3-01", "1.2.3+", "' '", "[]", "''", "null", ""} {
			dir := writeV2Source(t, fmt.Sprintf("schema_version: %d\nimage_version: %s\n", schema, value))
			if _, err := ParseV2(dir); err == nil {
				t.Errorf("accepted %s", value)
			}
		}
	}
}

func TestVersionUpdate(t *testing.T) {
	for _, tc := range []struct{ part, want string }{{"major", "2.0.0"}, {"minor", "1.3.0"}, {"patch", "1.2.4"}} {
		t.Run(tc.part, func(t *testing.T) {
			dir := writeV2Source(t, "# image\nschema_version: 2\nimage_version: '1.2.3-rc.1+build.7' # release\nprompts: [{runtime: identity}]\n")
			path := filepath.Join(dir, DefaultFilename)
			got, err := UpdateVersion(path, tc.part)
			if err != nil || got != tc.want {
				t.Fatalf("update = %q, %v", got, err)
			}
			got, err = GetVersion(dir)
			if err != nil || got != tc.want {
				t.Fatalf("get = %q, %v", got, err)
			}
			body, _ := os.ReadFile(path)
			for _, keep := range []string{"# image", "# release", "runtime: identity"} {
				if !strings.Contains(string(body), keep) {
					t.Fatalf("lost %q: %s", keep, body)
				}
			}
		})
	}
}

func TestVersionErrorsLeaveSourceUnchanged(t *testing.T) {
	for _, body := range []string{"schema_version: 2\n", "image_version: garbage\n", "image_version: 1.2.3\nimage_version: 2.3.4\n", "image_version: 1.2.3\n---\nother: document\n"} {
		dir := writeV2Source(t, body)
		if _, err := UpdateVersion(dir, "patch"); err == nil {
			t.Errorf("accepted %q", body)
		}
		got, _ := os.ReadFile(filepath.Join(dir, DefaultFilename))
		if string(got) != body {
			t.Fatalf("changed invalid source: %s", got)
		}
	}
}
