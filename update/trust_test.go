package update

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/blang/semver"
	pkgflags "github.com/komari-monitor/komari-agent/cmd/flags"
	"github.com/komari-monitor/komari-agent/internal/selfupdate"
)

func TestUpdaterConfigAlwaysValidatesTLS(t *testing.T) {
	previous := pkgflags.GlobalConfig.IgnoreUnsafeCert
	t.Cleanup(func() { pkgflags.GlobalConfig.IgnoreUnsafeCert = previous })
	pkgflags.GlobalConfig.IgnoreUnsafeCert = true
	transport, ok := updaterConfig().HTTPClient.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("updater must always use a TLS-verifying transport")
	}
}

func TestRepoSlugValidation(t *testing.T) {
	for _, slug := range []string{"", "owner", "owner/repo/extra", "../repo", "owner/..", "owner/repo?x=1", "owner/repo#x", "owner/repo%2fother", "owner\\repo/name", "owner /repo"} {
		if _, _, err := splitRepoSlug(slug); err == nil {
			t.Errorf("accepted invalid repo slug %q", slug)
		}
	}
	if owner, repo, err := splitRepoSlug("wugan666/komari-agent"); err != nil || owner != "wugan666" || repo != "komari-agent" {
		t.Fatalf("valid repository rejected: %s/%s, %v", owner, repo, err)
	}
}

func TestAssetURLMustBelongToOwnedRepository(t *testing.T) {
	asset := expectedAssetName(runtime.GOOS, runtime.GOARCH)
	valid := "https://github.com/wugan666/komari-agent/releases/download/v1.2.3/" + asset
	if err := validateAssetURL(valid, "wugan666", "komari-agent", asset, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(valid, "wugan666", "komari-monitor", 1),
		strings.Replace(valid, "https:", "http:", 1),
		strings.Replace(valid, "github.com", "github.com.attacker.example", 1),
		strings.Replace(valid, "github.com", "user@github.com", 1),
		strings.Replace(valid, "v1.2.3/", "v1.2.2/", 1),
		strings.Replace(valid, "v1.2.3/", "../", 1),
		valid + "?redirect=upstream", valid + "#fragment", valid + ".sha256",
	} {
		if err := validateAssetURL(bad, "wugan666", "komari-agent", asset, "v1.2.3"); err == nil {
			t.Errorf("accepted untrusted URL %q", bad)
		}
	}
}

func TestStableUpdateDoesNotDowngrade(t *testing.T) {
	for _, version := range []string{"1.2.3", "1.2.2"} {
		t.Run(version, func(t *testing.T) {
			updater := &fakeSelfUpdater{
				detectLatest: func(string) (*selfupdate.Release, bool, error) { return testStableRelease(version), true, nil },
				updateTo:     func(*selfupdate.Release, string) error { t.Fatal("must not replace current binary"); return nil },
			}
			if err := checkAndUpdateStable(semver.MustParse("1.2.3"), updater); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStableUpdateRejectsMissingChecksumAndUntrustedSource(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*selfupdate.Release)
	}{
		{"missing checksum", func(r *selfupdate.Release) { r.ValidationAssetID = -1 }},
		{"missing binary", func(r *selfupdate.Release) { r.AssetID = 0 }},
		{"upstream repository", func(r *selfupdate.Release) { r.RepoOwner = "komari-monitor" }},
		{"upstream asset URL", func(r *selfupdate.Release) {
			r.AssetURL = strings.Replace(r.AssetURL, "wugan666", "komari-monitor", 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			release := testStableRelease("1.2.4")
			test.alter(release)
			updater := &fakeSelfUpdater{
				detectLatest: func(string) (*selfupdate.Release, bool, error) { return release, true, nil },
				updateTo:     func(*selfupdate.Release, string) error { t.Fatal("must not download rejected update"); return nil },
			}
			if err := checkAndUpdateStable(semver.MustParse("1.2.3"), updater); err == nil {
				t.Fatal("expected update rejection")
			}
		})
	}
}

func TestSnapshotVersionOrdering(t *testing.T) {
	for _, test := range []struct {
		current, latest string
		want            bool
	}{
		{"Snapshot-2607061200", "Snapshot-2607061159", false},
		{"Snapshot-2607061200", "Snapshot-260706120001-100-1", true},
		{"Snapshot-260706120001-100-1", "Snapshot-2607061200", false},
		{"Snapshot-260706120001-100-1", "Snapshot-260706120001-100-1", false},
		{"Snapshot-260706120001-100-1", "Snapshot-260706120001-101-1", true},
		{"Snapshot-260706120001-100-1", "Snapshot-260706120001-100-2", true},
		{"Snapshot-260706120001-100-2", "Snapshot-260706120001-100-1", false},
		{"Snapshot-260706120001-100-1", "Snapshot-260706120000-999-9", false},
		{"Snapshot-invalid", "Snapshot-2607061200", false},
		{"Snapshot-2607061200", "Snapshot-2607321200", false},
		{"Snapshot-2607061200", "Snapshot-260706120001-0-1", false},
		{"Snapshot-2607061200", "Snapshot-260706120001-1-0", false},
	} {
		if got := snapshotNeedsUpdate(test.current, snapshotReleaseCandidate{TagName: test.latest}); got != test.want {
			t.Errorf("%s -> %s: got %v, want %v", test.current, test.latest, got, test.want)
		}
	}
}

func TestSnapshotIgnoresRepublishTime(t *testing.T) {
	asset := expectedAssetName(runtime.GOOS, runtime.GOARCH)
	releases := []githubRelease{
		testRelease("Snapshot-2607061200", true, false, time.Now(), asset),
		testRelease("Snapshot-2607061300", true, false, time.Now().Add(-time.Hour), asset),
	}
	latest, found := selectLatestSnapshotRelease(releases, asset)
	if !found || latest.TagName != "Snapshot-2607061300" {
		t.Fatalf("selected older republished snapshot: %+v", latest)
	}
}

func TestSnapshotRejectsMissingChecksumAndUntrustedSource(t *testing.T) {
	oldVersion := CurrentVersion
	CurrentVersion = "Snapshot-2607061100"
	t.Cleanup(func() { CurrentVersion = oldVersion })
	asset := expectedAssetName(runtime.GOOS, runtime.GOARCH)
	for _, test := range []struct {
		name  string
		alter func(*githubRelease)
	}{
		{"missing checksum", func(r *githubRelease) { r.Assets = r.Assets[:1] }},
		{"upstream binary", func(r *githubRelease) {
			r.Assets[0].BrowserDownloadURL = strings.Replace(r.Assets[0].BrowserDownloadURL, "wugan666", "komari-monitor", 1)
		}},
		{"upstream checksum", func(r *githubRelease) {
			r.Assets[1].BrowserDownloadURL = strings.Replace(r.Assets[1].BrowserDownloadURL, "wugan666", "komari-monitor", 1)
		}},
		{"checksum from another tag", func(r *githubRelease) {
			r.Assets[1].BrowserDownloadURL = strings.Replace(r.Assets[1].BrowserDownloadURL, "Snapshot-2607061200", "Snapshot-2607061150", 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			release := testRelease("Snapshot-2607061200", true, false, time.Now(), asset, asset+".sha256")
			test.alter(&release)
			updater := &fakeSelfUpdater{updateTo: func(*selfupdate.Release, string) error { t.Fatal("must not download rejected update"); return nil }}
			lister := func(string, string) ([]githubRelease, error) { return []githubRelease{release}, nil }
			if err := checkAndUpdateSnapshot(updater, lister, func() bool { return false }); err == nil {
				t.Fatal("expected update rejection")
			}
		})
	}
}

func TestChecksumValidator(t *testing.T) {
	binary := []byte("trusted binary")
	valid := fmt.Sprintf("%x", sha256.Sum256(binary))
	validator := updaterConfig().Validator
	if validator == nil || validator.Suffix() != ".sha256" {
		t.Fatal("updater must enable per-asset SHA256 verification")
	}
	for _, checksum := range []string{valid, valid + "  komari-agent-linux-amd64\n"} {
		if err := validator.Validate(binary, []byte(checksum)); err != nil {
			t.Fatal(err)
		}
	}
	for _, checksum := range []string{"", "short", strings.Repeat("z", 64), strings.Repeat("0", 64)} {
		if err := validator.Validate(binary, []byte(checksum)); err == nil {
			t.Errorf("accepted bad checksum %q", checksum)
		}
	}
}

// 使用真实 selfupdate 下载/验证流程，确认校验失败时原有可执行文件不会被覆盖。
func TestChecksumFailureLeavesBinaryUntouched(t *testing.T) {
	for _, checksum := range []string{"short", strings.Repeat("0", 64)} {
		t.Run(checksum, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				switch {
				case strings.HasSuffix(r.URL.Path, "/releases/assets/1"):
					_, _ = w.Write([]byte("new binary"))
				case strings.HasSuffix(r.URL.Path, "/releases/assets/2"):
					_, _ = w.Write([]byte(checksum))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			config := updaterConfig()
			config.APIToken = "test-only"
			config.APIBaseURL = server.URL + "/"
			updater, err := selfupdate.NewUpdater(config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "agent")
			if err := os.WriteFile(path, []byte("original binary"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := updater.UpdateTo(testStableRelease("1.2.4"), path); err == nil || !strings.Contains(err.Error(), "validating asset content") {
				t.Fatalf("expected checksum failure, got %v", err)
			}
			contents, err := os.ReadFile(path)
			if err != nil || string(contents) != "original binary" {
				t.Fatalf("original binary changed: %q, %v", contents, err)
			}
		})
	}
}

func TestStableDetectionRejectsMissingChecksum(t *testing.T) {
	asset := expectedAssetName(runtime.GOOS, runtime.GOARCH)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/"+Repo+"/releases") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]githubRelease{testRelease("v1.2.4", false, false, time.Now(), asset)})
	}))
	defer server.Close()
	config := updaterConfig()
	config.APIToken = "test-only"
	config.APIBaseURL = server.URL + "/"
	updater, err := selfupdate.NewUpdater(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := updater.DetectLatest(Repo); err == nil || !strings.Contains(err.Error(), ".sha256") {
		t.Fatalf("expected missing checksum failure, got %v", err)
	}
}
