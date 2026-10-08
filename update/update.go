package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/komari-monitor/komari-agent/dnsresolver"
	"github.com/komari-monitor/komari-agent/internal/selfupdate"
	"github.com/komari-monitor/komari-agent/utils"
)

var ErrRestartRequired = errors.New("update installed; restart required")

var (
	CurrentVersion string = "0.0.1"
	Repo           string = "wugan666/komari-agent"
)

const (
	snapshotVersionPrefix = "Snapshot-"
	containerMarkerPath   = "/.komari-agent-container"
	githubAPIBaseURL      = "https://api.github.com"
)

var (
	repoOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	repoNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	snapshotPattern  = regexp.MustCompile(`^Snapshot-([0-9]{10}|[0-9]{12}-[0-9]+-[0-9]+)$`)
	checksumPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type buildTrack int

const (
	stableTrack buildTrack = iota
	snapshotTrack
)

type githubRelease struct {
	TagName     string               `json:"tag_name"`
	Name        string               `json:"name"`
	Body        string               `json:"body"`
	Draft       bool                 `json:"draft"`
	Prerelease  bool                 `json:"prerelease"`
	HTMLURL     string               `json:"html_url"`
	PublishedAt time.Time            `json:"published_at"`
	Assets      []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int    `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type snapshotReleaseCandidate struct {
	TagName     string
	Name        string
	Body        string
	HTMLURL     string
	PublishedAt time.Time
	Asset       githubReleaseAsset
	Checksum    githubReleaseAsset
}

type selfUpdater interface {
	DetectLatest(slug string) (*selfupdate.Release, bool, error)
	UpdateTo(release *selfupdate.Release, cmdPath string) error
}

type releaseLister func(owner, repo string) ([]githubRelease, error)
type containerCheck func() bool

// parseVersion 解析可能带有 v/V 前缀，以及预发布或构建元数据的版本字符串
func parseVersion(ver string) (semver.Version, error) {
	ver = strings.TrimPrefix(ver, "v")
	ver = strings.TrimPrefix(ver, "V")
	return semver.ParseTolerant(ver)
}

// needUpdate 判断是否需要更新
func needUpdate(current, latest semver.Version) bool {
	// 返回最新版本大于当前版本时需要更新
	return latest.Compare(current) > 0
}

func detectBuildTrack(version string) buildTrack {
	if strings.HasPrefix(version, snapshotVersionPrefix) {
		return snapshotTrack
	}
	return stableTrack
}

func expectedAssetName(goos, goarch string) string {
	name := fmt.Sprintf("komari-agent-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func findReleaseAsset(release githubRelease, assetName string) (githubReleaseAsset, bool) {
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			return asset, true
		}
	}
	return githubReleaseAsset{}, false
}

func selectLatestSnapshotRelease(releases []githubRelease, assetName string) (snapshotReleaseCandidate, bool) {
	var latest snapshotReleaseCandidate
	found := false

	for _, release := range releases {
		if release.Draft || !release.Prerelease {
			continue
		}
		if _, err := parseSnapshotVersion(release.TagName); err != nil {
			continue
		}

		asset, ok := findReleaseAsset(release, assetName)
		if !ok {
			continue
		}
		checksum, _ := findReleaseAsset(release, assetName+".sha256")

		candidate := snapshotReleaseCandidate{
			TagName:     release.TagName,
			Name:        release.Name,
			Body:        release.Body,
			HTMLURL:     release.HTMLURL,
			PublishedAt: release.PublishedAt,
			Asset:       asset,
			Checksum:    checksum,
		}

		// 按构建标签排序，重新发布旧版本不能让客户端回退。
		if !found || snapshotNeedsUpdate(latest.TagName, candidate) {
			latest = candidate
			found = true
		}
	}

	return latest, found
}

func snapshotNeedsUpdate(currentVersion string, latest snapshotReleaseCandidate) bool {
	current, currentErr := parseSnapshotVersion(currentVersion)
	next, nextErr := parseSnapshotVersion(latest.TagName)
	return currentErr == nil && nextErr == nil && next.After(current)
}

type snapshotBuildVersion struct {
	createdAt time.Time
	runID     uint64
	attempt   uint64
}

func (v snapshotBuildVersion) After(other snapshotBuildVersion) bool {
	if !v.createdAt.Equal(other.createdAt) {
		return v.createdAt.After(other.createdAt)
	}
	if v.runID != other.runID {
		return v.runID > other.runID
	}
	return v.attempt > other.attempt
}

func parseSnapshotVersion(version string) (snapshotBuildVersion, error) {
	var result snapshotBuildVersion
	if !snapshotPattern.MatchString(version) {
		return result, fmt.Errorf("invalid snapshot version %q", version)
	}
	parts := strings.Split(strings.TrimPrefix(version, snapshotVersionPrefix), "-")
	layout := "0601021504"
	if len(parts) == 3 {
		layout = "060102150405"
		var err error
		result.runID, err = strconv.ParseUint(parts[1], 10, 64)
		if err != nil || result.runID == 0 {
			return result, fmt.Errorf("invalid snapshot run ID in %q", version)
		}
		result.attempt, err = strconv.ParseUint(parts[2], 10, 64)
		if err != nil || result.attempt == 0 {
			return result, fmt.Errorf("invalid snapshot run attempt in %q", version)
		}
	}
	var err error
	result.createdAt, err = time.Parse(layout, parts[0])
	return result, err
}

func isContainerAgent() bool {
	_, err := os.Stat(containerMarkerPath)
	return err == nil
}

func splitRepoSlug(slug string) (string, string, error) {
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || !repoOwnerPattern.MatchString(parts[0]) || !repoNamePattern.MatchString(parts[1]) {
		return "", "", fmt.Errorf("invalid repo slug %q, expected owner/name", slug)
	}
	return parts[0], parts[1], nil
}

func validateAssetURL(assetURL, owner, repo, assetName, tag string) error {
	u, err := url.Parse(assetURL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("untrusted update asset URL %q", assetURL)
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 6 || !strings.EqualFold(parts[0], owner) || !strings.EqualFold(parts[1], repo) ||
		parts[2] != "releases" || parts[3] != "download" || parts[4] == "" || parts[4] == "." || parts[4] == ".." ||
		parts[5] != assetName || (tag != "" && parts[4] != tag) {
		return fmt.Errorf("update asset must belong to %s/%s and match %s", owner, repo, assetName)
	}
	return nil
}

func validateUpdateRelease(release *selfupdate.Release, owner, repo string) error {
	if release == nil || !strings.EqualFold(release.RepoOwner, owner) || !strings.EqualFold(release.RepoName, repo) {
		return fmt.Errorf("update release must belong to %s/%s", owner, repo)
	}
	if release.AssetID <= 0 || release.ValidationAssetID <= 0 {
		return errors.New("update release is missing its binary or SHA256 checksum asset")
	}
	return validateAssetURL(release.AssetURL, owner, repo, expectedAssetName(runtime.GOOS, runtime.GOARCH), "")
}

// 依赖库的 SHA2Validator 会直接读取前 64 字节；先检查格式，避免损坏文件导致 panic。
type checksumValidator struct {
	selfupdate.SHA2Validator
}

func (v *checksumValidator) Validate(binary, checksum []byte) error {
	if len(checksum) < 64 || !checksumPattern.Match(checksum[:64]) {
		return errors.New("invalid SHA256 checksum file")
	}
	return v.SHA2Validator.Validate(binary, checksum)
}

func updaterConfig() selfupdate.Config {
	return selfupdate.Config{
		Validator:  &checksumValidator{},
		AssetName:  expectedAssetName(runtime.GOOS, runtime.GOARCH),
		HTTPClient: dnsresolver.GetVerifiedHTTPClient(60 * time.Second),
		APIToken:   os.Getenv("GITHUB_TOKEN"),
	}
}

func listGitHubReleases(owner, repo string) ([]githubRelease, error) {
	var releases []githubRelease

	for page := 1; page <= 20; page++ {
		endpoint := fmt.Sprintf(
			"%s/repos/%s/%s/releases?per_page=100&page=%d",
			githubAPIBaseURL,
			url.PathEscape(owner),
			url.PathEscape(repo),
			page,
		)
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create GitHub releases request: %w", err)
		}

		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "komari-agent")
		if token := os.Getenv("GITHUB_TOKEN"); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := dnsresolver.GetVerifiedHTTPClient(60 * time.Second).Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to list GitHub releases: %w", utils.SanitizeHTTPError(err))
		}

		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("GitHub releases API returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var pageReleases []githubRelease
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&pageReleases); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("failed to decode GitHub releases response: %w", err)
		}
		_ = resp.Body.Close()

		releases = append(releases, pageReleases...)
		if len(pageReleases) < 100 {
			return releases, nil
		}
	}
	return nil, errors.New("release listing exceeds 20 pages")
}

func currentExecutablePath() (string, error) {
	cmdPath, err := os.Executable()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(cmdPath, ".exe") {
		cmdPath += ".exe"
	}

	stat, err := os.Lstat(cmdPath)
	if err != nil {
		return "", fmt.Errorf("failed to stat %q: %w", cmdPath, err)
	}
	if stat.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(cmdPath)
		if err != nil {
			return "", fmt.Errorf("failed to resolve symlink %q for executable: %w", cmdPath, err)
		}
		cmdPath = resolved
	}

	return cmdPath, nil
}

func selfUpdateReleaseFromSnapshot(owner, repo string, candidate snapshotReleaseCandidate) *selfupdate.Release {
	publishedAt := candidate.PublishedAt
	return &selfupdate.Release{
		Version:           semver.Version{},
		AssetURL:          candidate.Asset.BrowserDownloadURL,
		AssetByteSize:     candidate.Asset.Size,
		AssetID:           candidate.Asset.ID,
		ValidationAssetID: candidate.Checksum.ID,
		URL:               candidate.HTMLURL,
		ReleaseNotes:      candidate.Body,
		Name:              candidate.Name,
		PublishedAt:       &publishedAt,
		RepoOwner:         owner,
		RepoName:          repo,
	}
}

func DoUpdateWorks(onRestartRequired func()) {
	ticker_ := time.NewTicker(time.Duration(6) * time.Hour)
	defer ticker_.Stop()
	runScheduledUpdates(ticker_.C, CheckAndUpdate, onRestartRequired)
}

func runScheduledUpdates(ticks <-chan time.Time, check func() error, onRestartRequired func()) {
	for range ticks {
		if errors.Is(check(), ErrRestartRequired) {
			if onRestartRequired != nil {
				onRestartRequired()
			}
			return
		}
	}
}

func checkAndUpdateStable(currentSemVer semver.Version, updater selfUpdater) error {
	owner, repo, err := splitRepoSlug(Repo)
	if err != nil {
		return err
	}
	latest, found, err := updater.DetectLatest(Repo)
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}

	if !found {
		log.Println("No stable release was found in", Repo)
		return nil
	}
	if err := validateUpdateRelease(latest, owner, repo); err != nil {
		return err
	}
	if !needUpdate(currentSemVer, latest.Version) {
		log.Println("Current version is the latest or newer:", CurrentVersion)
		return nil
	}
	cmdPath, err := currentExecutablePath()
	if err != nil {
		return fmt.Errorf("failed to resolve current executable path: %w", err)
	}
	if err := updater.UpdateTo(latest, cmdPath); err != nil {
		return fmt.Errorf("failed to update to version %s: %w", latest.Version, err)
	}
	log.Printf("Successfully updated to version %s\n", latest.Version)
	return ErrRestartRequired
}

func checkAndUpdateSnapshot(updater selfUpdater, listReleases releaseLister, isContainer containerCheck) error {
	if isContainer() {
		log.Println("Snapshot agent is running in a container; skip binary self-update. Refresh the ghcr.io image tagged 'snapshot' instead.")
		return nil
	}

	owner, repo, err := splitRepoSlug(Repo)
	if err != nil {
		return err
	}
	if _, err := parseSnapshotVersion(CurrentVersion); err != nil {
		return err
	}

	releases, err := listReleases(owner, repo)
	if err != nil {
		return err
	}

	assetName := expectedAssetName(runtime.GOOS, runtime.GOARCH)
	latest, found := selectLatestSnapshotRelease(releases, assetName)
	if !found {
		log.Printf("No suitable snapshot release asset was found for %s. Current snapshot is considered up-to-date.", assetName)
		return nil
	}

	if !snapshotNeedsUpdate(CurrentVersion, latest) {
		log.Println("Current snapshot version is the latest or newer:", CurrentVersion)
		return nil
	}
	release := selfUpdateReleaseFromSnapshot(owner, repo, latest)
	if err := validateUpdateRelease(release, owner, repo); err != nil {
		return err
	}
	if err := validateAssetURL(latest.Asset.BrowserDownloadURL, owner, repo, assetName, latest.TagName); err != nil {
		return err
	}
	if err := validateAssetURL(latest.Checksum.BrowserDownloadURL, owner, repo, assetName+".sha256", latest.TagName); err != nil {
		return err
	}

	cmdPath, err := currentExecutablePath()
	if err != nil {
		return fmt.Errorf("failed to resolve current executable path: %w", err)
	}

	log.Printf("Will update %s from snapshot %s to %s\n", cmdPath, CurrentVersion, latest.TagName)
	if err := updater.UpdateTo(release, cmdPath); err != nil {
		return fmt.Errorf("failed to update to snapshot %s: %w", latest.TagName, err)
	}

	log.Printf("Successfully updated to snapshot version %s\n", latest.TagName)
	return ErrRestartRequired
}

// 检查更新并执行自动更新
func CheckAndUpdate() error {
	log.Println("Checking update...")
	if _, _, err := splitRepoSlug(Repo); err != nil {
		return err
	}

	updater, err := selfupdate.NewUpdater(updaterConfig())
	if err != nil {
		return fmt.Errorf("failed to create updater: %v", err)
	}

	if detectBuildTrack(CurrentVersion) == snapshotTrack {
		return checkAndUpdateSnapshot(updater, listGitHubReleases, isContainerAgent)
	}

	currentSemVer, err := parseVersion(CurrentVersion)
	if err != nil {
		return fmt.Errorf("failed to parse current version: %v", err)
	}

	return checkAndUpdateStable(currentSemVer, updater)
}
