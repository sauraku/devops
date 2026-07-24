package composepolicy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotInputsUsesPrivateStableCopies(t *testing.T) {
	root := t.TempDir()
	compose := writeCompose(t, root, "services:\n  app:\n    image: example/app:v1\n")
	env := filepath.Join(root, ".env.main")
	if err := os.WriteFile(env, []byte("IMAGE_TAG=v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := os.Chmod(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	composeSnapshot, envSnapshot, err := SnapshotInputs(compose, env, root, destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compose, []byte("services:\n  app:\n    build: /\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte("IMAGE_TAG=changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotCompose, _ := os.ReadFile(composeSnapshot)
	gotEnv, _ := os.ReadFile(envSnapshot)
	if string(gotCompose) != "services:\n  app:\n    image: example/app:v1\n" || string(gotEnv) != "IMAGE_TAG=v1\n" {
		t.Fatalf("snapshots changed with project files: compose=%q env=%q", gotCompose, gotEnv)
	}
}

func TestSnapshotInputsRejectsEnvironmentSymlinkOutsideProject(t *testing.T) {
	root := t.TempDir()
	compose := writeCompose(t, root, "services:\n  app:\n    image: example/app\n")
	outside := filepath.Join(t.TempDir(), "controller.env")
	if err := os.WriteFile(outside, []byte("DEPLOY_CONTROL_TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(root, ".env.main")
	if err := os.Symlink(outside, env); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := os.Chmod(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SnapshotInputs(compose, env, root, destination); err == nil {
		t.Fatal("environment symlink outside project was accepted")
	}
}
