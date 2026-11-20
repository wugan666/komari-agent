package update

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"time"

	pkgflags "github.com/komari-monitor/komari-agent/cmd/flags"
	"github.com/komari-monitor/komari-agent/dnsresolver"
)

// The panel this agent reports to already has full control of the agent, so
// it is also the preferred source of agent binaries: it serves the build it
// was released with under /download/agent. GitHub is only consulted when the
// panel does not host a binary for this platform.

const controllerAssetID = int64(-1)

var (
	controllerVersionPattern  = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)
	controllerChecksumPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func controllerBase() string {
	endpoint := strings.TrimRight(strings.TrimSpace(pkgflags.GlobalConfig.Endpoint), "/")
	if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		return ""
	}
	return endpoint + "/download/agent"
}

func controllerGet(url string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "komari-agent")
	resp, err := dnsresolver.GetHTTPClient(60 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

// controllerRelease describes the single agent build hosted by the panel as a
// release, so the regular version-selection rules apply to it unchanged.
func controllerRelease(assetName string) (githubRelease, bool) {
	base := controllerBase()
	if base == "" {
		return githubRelease{}, false
	}
	rawVersion, err := controllerGet(base+"/VERSION", 128)
	if err != nil {
		return githubRelease{}, false
	}
	version := strings.TrimSpace(string(rawVersion))
	rawChecksum, err := controllerGet(base+"/"+assetName+".sha256", 4096)
	if err != nil || !controllerVersionPattern.MatchString(version) {
		return githubRelease{}, false
	}
	fields := strings.Fields(string(rawChecksum))
	if len(fields) == 0 || !controllerChecksumPattern.MatchString(fields[0]) {
		log.Printf("WARNING: panel serves an invalid checksum for %s; ignoring the panel as update source", assetName)
		return githubRelease{}, false
	}
	return githubRelease{
		TagName:     version,
		PublishedAt: time.Now().UTC(),
		Assets: []githubReleaseAsset{{
			ID:                 controllerAssetID,
			Name:               assetName,
			Digest:             "sha256:" + fields[0],
			BrowserDownloadURL: base + "/" + assetName,
		}},
	}, true
}

func listReleases(owner, repo string) ([]githubRelease, error) {
	if release, ok := controllerRelease(expectedAssetName(runtime.GOOS, runtime.GOARCH)); ok {
		return []githubRelease{release}, nil
	}
	return listGitHubReleases(owner, repo)
}

func downloadControllerAsset(asset githubReleaseAsset) ([]byte, error) {
	data, err := controllerGet(asset.BrowserDownloadURL, maxReleaseAssetSize)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty agent binary")
	}
	return data, nil
}
