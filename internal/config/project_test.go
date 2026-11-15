package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadProjectConfigIncludesSharedPathSettings(t *testing.T) {
	root := t.TempDir()
	data := []byte(`{"dependency_dirs":["vendor"],"shared_paths":["config/local.json"],"exclude_shared_paths":[".env.test"]}`)
	if err := os.WriteFile(filepath.Join(root, ".grove.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProjectConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.SharedPaths, []string{"config/local.json"}) || !reflect.DeepEqual(got.ExcludeSharedPaths, []string{".env.test"}) {
		t.Fatalf("shared path config = (%v, %v)", got.SharedPaths, got.ExcludeSharedPaths)
	}
}
