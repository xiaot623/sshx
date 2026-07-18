package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCreatesOneBinaryShimsAndPreservesSettings(t *testing.T) {
	home := t.TempDir()
	tools := t.TempDir()
	ssh := filepath.Join(tools, "ssh")
	scp := filepath.Join(tools, "scp")
	writeExecutable(t, ssh, "#!/bin/sh\nif [ \"${1:-}\" = -V ]; then echo 'OpenSSH_9.9 test' >&2; exit 0; fi\nexit 0\n")
	writeExecutable(t, scp, "#!/bin/sh\necho 'usage: scp test' >&2\nexit 1\n")
	driver := filepath.Join(tools, "sshx-driver")
	writeExecutable(t, driver, fmt.Sprintf("#!/bin/sh\ncase \"${0##*/}\" in ssh) exec %s \"$@\" ;; scp) exec %s \"$@\" ;; esac\nexit 2\n", shellTestQuote(ssh), shellTestQuote(scp)))
	settings, err := SettingsPath(VSCode, InstallOptions{HomeDir: home, ConfigHome: filepath.Join(home, "config"), GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "{\n  // retained\n  \"editor.fontSize\": 15,\n}\n"
	if err := os.WriteFile(settings, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	opts := InstallOptions{
		HomeDir: home, ConfigHome: filepath.Join(home, "config"), GOOS: "linux", Executable: driver, NPMManaged: true,
		LookPath: func(name string) (string, error) {
			if name == "ssh" {
				return ssh, nil
			}
			return scp, nil
		},
	}
	result, err := Install(context.Background(), VSCode, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ssh", "scp"} {
		path := filepath.Join(filepath.Dir(result.SSHShim), name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is not a symlink: %v, %v", path, info, err)
		}
		target, err := os.Readlink(path)
		if err != nil || target != driver {
			t.Fatalf("%s -> %q, %v", path, target, err)
		}
	}
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "// retained") || !strings.Contains(string(b), "\"editor.fontSize\": 15") {
		t.Fatalf("settings were not preserved:\n%s", b)
	}
	value, found, err := StringProperty(b, settingsKey)
	if err != nil || !found || value != result.SSHShim {
		t.Fatalf("remote.SSH.path = %q, %v, %v", value, found, err)
	}
	if mode := fileMode(t, settings); mode != 0o640 {
		t.Fatalf("settings mode = %#o", mode)
	}
	marker := filepath.Join(filepath.Dir(filepath.Dir(result.SSHShim)), npmManagedMarker)
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "1\n" {
		t.Fatalf("npm marker = %q, %v", contents, err)
	}
	if mode := fileMode(t, marker); mode != 0o600 {
		t.Fatalf("npm marker mode = %#o", mode)
	}
	// Re-running install is the repair and upgrade operation.
	second, err := Install(context.Background(), VSCode, opts)
	if err != nil || second.SSHShim != result.SSHShim {
		t.Fatalf("idempotent install = %#v, %v", second, err)
	}
}

func TestInstallSelfCheckFailureLeavesSettingsUntouched(t *testing.T) {
	home := t.TempDir()
	tools := t.TempDir()
	ssh := filepath.Join(tools, "ssh")
	scp := filepath.Join(tools, "scp")
	driver := filepath.Join(tools, "driver")
	writeExecutable(t, ssh, "#!/bin/sh\necho OpenSSH_good >&2\n")
	writeExecutable(t, scp, "#!/bin/sh\necho usage >&2\nexit 1\n")
	writeExecutable(t, driver, "#!/bin/sh\necho broken >&2\n")
	configHome := filepath.Join(home, "config")
	settings, _ := SettingsPath(Cursor, InstallOptions{HomeDir: home, ConfigHome: configHome, GOOS: "linux"})
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\n  \"remote.SSH.path\": \"/original\"\n}\n")
	if err := os.WriteFile(settings, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Install(context.Background(), Cursor, InstallOptions{
		HomeDir: home, ConfigHome: configHome, GOOS: "linux", Executable: driver,
		LookPath: func(name string) (string, error) {
			if name == "ssh" {
				return ssh, nil
			}
			return scp, nil
		},
	})
	if err == nil {
		t.Fatal("expected self-check failure")
	}
	got, readErr := os.ReadFile(settings)
	if readErr != nil || string(got) != string(original) {
		t.Fatalf("settings changed after failure: %q, %v", got, readErr)
	}
}

func TestInstallRollsBackCommittedShimWhenSettingsWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission failure cannot be induced as root")
	}
	home := t.TempDir()
	tools := t.TempDir()
	ssh := filepath.Join(tools, "ssh")
	scp := filepath.Join(tools, "scp")
	writeExecutable(t, ssh, "#!/bin/sh\necho OpenSSH_test >&2\n")
	writeExecutable(t, scp, "#!/bin/sh\necho usage >&2\nexit 1\n")
	driverOne := filepath.Join(tools, "driver-one")
	driverTwo := filepath.Join(tools, "driver-two")
	driverScript := fmt.Sprintf("#!/bin/sh\ncase \"${0##*/}\" in ssh) exec %s \"$@\" ;; scp) exec %s \"$@\" ;; esac\n", shellTestQuote(ssh), shellTestQuote(scp))
	writeExecutable(t, driverOne, driverScript)
	writeExecutable(t, driverTwo, driverScript)
	configHome := filepath.Join(home, "config")
	base := InstallOptions{
		HomeDir: home, ConfigHome: configHome, GOOS: "linux",
		LookPath: func(name string) (string, error) {
			if name == "ssh" {
				return ssh, nil
			}
			return scp, nil
		},
	}
	base.Executable = driverOne
	result, err := Install(context.Background(), VSCode, base)
	if err != nil {
		t.Fatal(err)
	}
	settingsDir := filepath.Dir(result.SettingsPath)
	if err := os.Chmod(settingsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(settingsDir, 0o700)
	base.Executable = driverTwo
	_, installErr := Install(context.Background(), VSCode, base)
	if err := os.Chmod(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if installErr == nil {
		t.Fatal("expected settings write failure")
	}
	target, err := os.Readlink(result.SSHShim)
	if err != nil || target != driverOne {
		t.Fatalf("rollback shim -> %q, %v; want %q", target, err, driverOne)
	}
}

func TestIntegrationShimChainIsUnwrappedAndCyclesAreRejected(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(binDir, "ssh")
	writeExecutable(t, shim, "#!/bin/sh\nexit 1\n")
	upstream := filepath.Join(t.TempDir(), "ssh")
	writeExecutable(t, upstream, "#!/bin/sh\nexit 0\n")
	descriptorPath := filepath.Join(root, "integration.json")
	descriptor := Descriptor{Schema: 1, Profile: VSCode, SSHPath: upstream, SCPPath: upstream}
	if err := WriteDescriptor(descriptorPath, descriptor); err != nil {
		t.Fatal(err)
	}
	got, err := unwrapIntegrationClient(shim, false)
	if err != nil || !samePath(got, upstream) {
		t.Fatalf("unwrapped path = %q, %v", got, err)
	}
	descriptor.SSHPath = shim
	if err := WriteDescriptor(descriptorPath, descriptor); err != nil {
		t.Fatal(err)
	}
	if _, err := unwrapIntegrationClient(shim, false); err == nil {
		t.Fatal("recursive integration shim was accepted")
	}
}

func TestDescriptorSupportsSafeThirdPartyProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integration.json")
	descriptor := Descriptor{Schema: 1, Profile: "third-party_app", SSHPath: "/usr/bin/ssh", SCPPath: "/usr/bin/scp"}
	if err := WriteDescriptor(path, descriptor); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDescriptor(path)
	if err != nil || got.Profile != descriptor.Profile {
		t.Fatalf("descriptor = %#v, %v", got, err)
	}

	for _, profile := range []Profile{"../escape", "..", ".hidden", "UPPERCASE"} {
		descriptor.Profile = profile
		if err := WriteDescriptor(path, descriptor); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadDescriptor(path); err == nil {
			t.Fatalf("unsafe integration profile %q was accepted", profile)
		}
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
