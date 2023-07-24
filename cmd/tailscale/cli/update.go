// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/peterbourgon/ff/v3/ffcli"
	"tailscale.com/net/tshttpproxy"
	"tailscale.com/util/must"
	"tailscale.com/util/winutil"
	"tailscale.com/version"
	"tailscale.com/version/distro"
)

var updateCmd = &ffcli.Command{
	Name:       "update",
	ShortUsage: "update",
	ShortHelp:  "[ALPHA] Update Tailscale to the latest/different version",
	Exec:       runUpdate,
	FlagSet: (func() *flag.FlagSet {
		fs := newFlagSet("update")
		fs.BoolVar(&updateArgs.yes, "yes", false, "update without interactive prompts")
		fs.BoolVar(&updateArgs.dryRun, "dry-run", false, "print what update would do without doing it, or prompts")
		fs.BoolVar(&updateArgs.appStore, "app-store", false, "HIDDEN: check the App Store for updates, even if this is not an App Store install (for testing only)")
		// These flags are not supported on Arch-based installs. Arch only
		// offers one variant of tailscale and it's always the latest version.
		if distro.Get() != distro.Arch {
			fs.StringVar(&updateArgs.track, "track", "", `which track to check for updates: "stable" or "unstable" (dev); empty means same as current`)
			fs.StringVar(&updateArgs.version, "version", "", `explicit version to update/downgrade to`)
		}
		return fs
	})(),
}

var updateArgs struct {
	yes      bool
	dryRun   bool
	appStore bool
	track    string // explicit track; empty means same as current
	version  string // explicit version; empty means auto
}

// winMSIEnv is the environment variable that, if set, is the MSI file for the
// update command to install. It's passed like this so we can stop the
// tailscale.exe process from running before the msiexec process runs and tries
// to overwrite ourselves.
const winMSIEnv = "TS_UPDATE_WIN_MSI"

func runUpdate(ctx context.Context, args []string) error {
	if msi := os.Getenv(winMSIEnv); msi != "" {
		log.Printf("installing %v ...", msi)
		if err := installMSI(msi); err != nil {
			log.Printf("MSI install failed: %v", err)
			return err
		}
		log.Printf("success.")
		return nil
	}
	if len(args) > 0 {
		return flag.ErrHelp
	}
	if updateArgs.version != "" && updateArgs.track != "" {
		return errors.New("cannot specify both --version and --track")
	}
	up, err := newUpdater()
	if err != nil {
		return err
	}
	return up.update()
}

func versionIsStable(v string) (stable, wellFormed bool) {
	_, rest, ok := strings.Cut(v, ".")
	if !ok {
		return false, false
	}
	minorStr, _, ok := strings.Cut(rest, ".")
	if !ok {
		return false, false
	}
	minor, err := strconv.Atoi(minorStr)
	if err != nil {
		return false, false
	}
	return minor%2 == 0, true
}

func newUpdater() (*updater, error) {
	up := &updater{
		track: updateArgs.track,
	}
	switch up.track {
	case "stable", "unstable":
	case "":
		if version.IsUnstableBuild() {
			up.track = "unstable"
		} else {
			up.track = "stable"
		}
		if updateArgs.version != "" {
			stable, ok := versionIsStable(updateArgs.version)
			if !ok {
				return nil, fmt.Errorf("malformed version %q", updateArgs.version)
			}
			if stable {
				up.track = "stable"
			} else {
				up.track = "unstable"
			}
		}
	default:
		return nil, fmt.Errorf("unknown track %q; must be 'stable' or 'unstable'", up.track)
	}
	switch runtime.GOOS {
	case "windows":
		up.update = up.updateWindows
	case "linux":
		switch distro.Get() {
		case distro.Synology:
			up.update = up.updateSynology
		case distro.Debian: // includes Ubuntu
			up.update = up.updateDebLike
		case distro.Arch:
			up.update = up.updateArchLike
		}
		// TODO(awly): add support for Alpine
		switch {
		case haveExecutable("pacman"):
			up.update = up.updateArchLike
		case haveExecutable("apt-get"): // TODO(awly): add support for "apt"
			// The distro.Debian switch case above should catch most apt-based
			// systems, but add this fallback just in case.
			up.update = up.updateDebLike
		case haveExecutable("dnf"):
			up.update = up.updateFedoraLike("dnf")
		case haveExecutable("yum"):
			up.update = up.updateFedoraLike("yum")
		}
	case "darwin":
		switch {
		case !updateArgs.appStore && !version.IsSandboxedMacOS():
			return nil, errors.New("The 'update' command is not yet supported on this platform; see https://github.com/tailscale/tailscale/wiki/Tailscaled-on-macOS/ for now")
		case !updateArgs.appStore && strings.HasSuffix(os.Getenv("HOME"), "/io.tailscale.ipn.macsys/Data"):
			up.update = up.updateMacSys
		default:
			up.update = up.updateMacAppStore
		}
	}
	if up.update == nil {
		return nil, errors.New("The 'update' command is not supported on this platform; see https://tailscale.com/s/client-updates")
	}
	return up, nil
}

type updater struct {
	track  string
	update func() error
}

func (up *updater) currentOrDryRun(ver string) bool {
	if version.Short() == ver {
		fmt.Printf("already running %v; no update needed\n", ver)
		return true
	}
	if updateArgs.dryRun {
		fmt.Printf("Current: %v, Latest: %v\n", version.Short(), ver)
		return true
	}
	return false
}

var errUserAborted = errors.New("aborting update")

func (up *updater) confirm(ver string) error {
	if updateArgs.yes {
		log.Printf("Updating Tailscale from %v to %v; --yes given, continuing without prompts.\n", version.Short(), ver)
		return nil
	}

	fmt.Printf("This will update Tailscale from %v to %v. Continue? [y/n] ", version.Short(), ver)
	var resp string
	fmt.Scanln(&resp)
	resp = strings.ToLower(resp)
	switch resp {
	case "y", "yes", "sure":
		return nil
	}
	return errUserAborted
}

func (up *updater) updateSynology() error {
	// TODO(bradfitz): detect, map GOARCH+CPU to the right Synology arch.
	// TODO(bradfitz): add pkgs.tailscale.com endpoint to get release info
	// TODO(bradfitz): require root/sudo
	// TODO(bradfitz): run /usr/syno/bin/synopkg install tailscale.spk
	return errors.New("The 'update' command is not yet implemented on Synology.")
}

func (up *updater) updateDebLike() error {
	ver, err := requestedTailscaleVersion(updateArgs.version, up.track)
	if err != nil {
		return err
	}
	if up.currentOrDryRun(ver) {
		return nil
	}

	if err := requireRoot(); err != nil {
		return err
	}

	if updated, err := updateDebianAptSourcesList(up.track); err != nil {
		return err
	} else if updated {
		fmt.Printf("Updated %s to use the %s track\n", aptSourcesFile, up.track)
	}

	cmd := exec.Command("apt-get", "update",
		// Only update the tailscale repo, not the other ones, treating
		// the tailscale.list file as the main "sources.list" file.
		"-o", "Dir::Etc::SourceList=sources.list.d/tailscale.list",
		// Disable the "sources.list.d" directory:
		"-o", "Dir::Etc::SourceParts=-",
		// Don't forget about packages in the other repos just because
		// we're not updating them:
		"-o", "APT::Get::List-Cleanup=0",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	cmd = exec.Command("apt-get", "install", "--yes", "--allow-downgrades", "tailscale="+ver)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	return nil
}

const aptSourcesFile = "/etc/apt/sources.list.d/tailscale.list"

// updateDebianAptSourcesList updates the /etc/apt/sources.list.d/tailscale.list
// file to make sure it has the provided track (stable or unstable) in it.
//
// If it already has the right track (including containing both stable and
// unstable), it does nothing.
func updateDebianAptSourcesList(dstTrack string) (rewrote bool, err error) {
	was, err := os.ReadFile(aptSourcesFile)
	if err != nil {
		return false, err
	}
	newContent, err := updateDebianAptSourcesListBytes(was, dstTrack)
	if err != nil {
		return false, err
	}
	if bytes.Equal(was, newContent) {
		return false, nil
	}
	return true, os.WriteFile(aptSourcesFile, newContent, 0644)
}

func updateDebianAptSourcesListBytes(was []byte, dstTrack string) (newContent []byte, err error) {
	trackURLPrefix := []byte("https://pkgs.tailscale.com/" + dstTrack + "/")
	var buf bytes.Buffer
	var changes int
	bs := bufio.NewScanner(bytes.NewReader(was))
	hadCorrect := false
	commentLine := regexp.MustCompile(`^\s*\#`)
	pkgsURL := regexp.MustCompile(`\bhttps://pkgs\.tailscale\.com/((un)?stable)/`)
	for bs.Scan() {
		line := bs.Bytes()
		if !commentLine.Match(line) {
			line = pkgsURL.ReplaceAllFunc(line, func(m []byte) []byte {
				if bytes.Equal(m, trackURLPrefix) {
					hadCorrect = true
				} else {
					changes++
				}
				return trackURLPrefix
			})
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if hadCorrect || (changes == 1 && bytes.Equal(bytes.TrimSpace(was), bytes.TrimSpace(buf.Bytes()))) {
		// Unchanged or close enough.
		return was, nil
	}
	if changes != 1 {
		// No changes, or an unexpected number of changes (what?). Bail.
		// They probably editted it by hand and we don't know what to do.
		return nil, fmt.Errorf("unexpected/unsupported %s contents", aptSourcesFile)
	}
	return buf.Bytes(), nil
}

func (up *updater) updateArchLike() (err error) {
	if err := requireRoot(); err != nil {
		return err
	}

	defer func() {
		if err != nil && !errors.Is(err, errUserAborted) {
			err = fmt.Errorf(`%w; you can try updating using "pacman --sync --refresh tailscale"`, err)
		}
	}()

	out, err := exec.Command("pacman", "--sync", "--refresh", "--info", "tailscale").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed checking pacman for latest tailscale version: %w, output: %q", err, out)
	}
	ver, err := parsePacmanVersion(out)
	if err != nil {
		return err
	}
	if up.currentOrDryRun(ver) {
		return nil
	}
	if err := up.confirm(ver); err != nil {
		return err
	}

	cmd := exec.Command("pacman", "--sync", "--noconfirm", "tailscale")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed tailscale update using pacman: %w", err)
	}
	return nil
}

func parsePacmanVersion(out []byte) (string, error) {
	for _, line := range strings.Split(string(out), "\n") {
		// The line we're looking for looks like this:
		// Version         : 1.44.2-1
		if !strings.HasPrefix(line, "Version") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("version output from pacman is malformed: %q, cannot determine upgrade version", line)
		}
		ver := strings.TrimSpace(parts[1])
		// Trim the Arch patch version.
		ver = strings.Split(ver, "-")[0]
		if ver == "" {
			return "", fmt.Errorf("version output from pacman is malformed: %q, cannot determine upgrade version", line)
		}
		return ver, nil
	}
	return "", fmt.Errorf("could not find latest version of tailscale via pacman")
}

const yumRepoConfigFile = "/etc/yum.repos.d/tailscale.repo"

// updateFedoraLike updates tailscale on any distros in the Fedora family,
// specifically anything that uses "dnf" or "yum" package managers. The actual
// package manager is passed via packageManager.
func (up *updater) updateFedoraLike(packageManager string) func() error {
	return func() (err error) {
		if err := requireRoot(); err != nil {
			return err
		}
		defer func() {
			if err != nil && !errors.Is(err, errUserAborted) {
				err = fmt.Errorf(`%w; you can try updating using "%s upgrade tailscale"`, err, packageManager)
			}
		}()

		ver, err := requestedTailscaleVersion(updateArgs.version, up.track)
		if err != nil {
			return err
		}
		if up.currentOrDryRun(ver) {
			return nil
		}
		if err := up.confirm(ver); err != nil {
			return err
		}

		if updated, err := updateYUMRepoTrack(yumRepoConfigFile, up.track); err != nil {
			return err
		} else if updated {
			fmt.Printf("Updated %s to use the %s track\n", yumRepoConfigFile, up.track)
		}

		cmd := exec.Command(packageManager, "install", "--assumeyes", fmt.Sprintf("tailscale-%s-1", ver))
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		return nil
	}
}

// updateYUMRepoTrack updates the repoFile file to make sure it has the
// provided track (stable or unstable) in it.
func updateYUMRepoTrack(repoFile, dstTrack string) (rewrote bool, err error) {
	was, err := os.ReadFile(repoFile)
	if err != nil {
		return false, err
	}

	urlRe := regexp.MustCompile(`^(baseurl|gpgkey)=https://pkgs\.tailscale\.com/(un)?stable/`)
	urlReplacement := fmt.Sprintf("$1=https://pkgs.tailscale.com/%s/", dstTrack)

	s := bufio.NewScanner(bytes.NewReader(was))
	newContent := bytes.NewBuffer(make([]byte, 0, len(was)))
	for s.Scan() {
		line := s.Text()
		// Handle repo section name, like "[tailscale-stable]".
		if len(line) > 0 && line[0] == '[' {
			if !strings.HasPrefix(line, "[tailscale-") {
				return false, fmt.Errorf("%q does not look like a tailscale repo file, it contains an unexpected %q section", repoFile, line)
			}
			fmt.Fprintf(newContent, "[tailscale-%s]\n", dstTrack)
			continue
		}
		// Update the track mentioned in repo name.
		if strings.HasPrefix(line, "name=") {
			fmt.Fprintf(newContent, "name=Tailscale %s\n", dstTrack)
			continue
		}
		// Update the actual repo URLs.
		if strings.HasPrefix(line, "baseurl=") || strings.HasPrefix(line, "gpgkey=") {
			fmt.Fprintln(newContent, urlRe.ReplaceAllString(line, urlReplacement))
			continue
		}
		fmt.Fprintln(newContent, line)
	}
	if bytes.Equal(was, newContent.Bytes()) {
		return false, nil
	}
	return true, os.WriteFile(repoFile, newContent.Bytes(), 0644)
}

func (up *updater) updateMacSys() error {
	// use sparkle? do we have permissions from this context? does sudo help?
	// We can at least fail with a command they can run to update from the shell.
	// Like "tailscale update --macsys | sudo sh" or something.
	//
	// TODO(bradfitz,mihai): implement. But for now:
	return errors.New("The 'update' command is not yet implemented on macOS.")
}

func (up *updater) updateMacAppStore() error {
	out, err := exec.Command("defaults", "read", "/Library/Preferences/com.apple.commerce.plist", "AutoUpdate").CombinedOutput()
	if err != nil {
		return fmt.Errorf("can't check App Store auto-update setting: %w, output: %q", err, string(out))
	}
	const on = "1\n"
	if string(out) != on {
		fmt.Fprintln(os.Stderr, "NOTE: Automatic updating for App Store apps is turned off. You can change this setting in System Settings (search for ‘update’).")
	}

	out, err = exec.Command("softwareupdate", "--list").CombinedOutput()
	if err != nil {
		return fmt.Errorf("can't check App Store for available updates: %w, output: %q", err, string(out))
	}

	newTailscale := parseSoftwareupdateList(out)
	if newTailscale == "" {
		fmt.Println("no Tailscale update available")
		return nil
	}

	newTailscaleVer := strings.TrimPrefix(newTailscale, "Tailscale-")
	if up.currentOrDryRun(newTailscaleVer) {
		return nil
	}
	if err := up.confirm(newTailscaleVer); err != nil {
		return err
	}

	cmd := exec.Command("sudo", "softwareupdate", "--install", newTailscale)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("can't install App Store update for Tailscale: %w", err)
	}
	return nil
}

var macOSAppStoreListPattern = regexp.MustCompile(`(?m)^\s+\*\s+Label:\s*(Tailscale-\d[\d\.]+)`)

// parseSoftwareupdateList searches the output of `softwareupdate --list` on
// Darwin and returns the matching Tailscale package label. If there is none,
// returns the empty string.
//
// See TestParseSoftwareupdateList for example inputs.
func parseSoftwareupdateList(stdout []byte) string {
	matches := macOSAppStoreListPattern.FindSubmatch(stdout)
	if len(matches) < 2 {
		return ""
	}
	return string(matches[1])
}

var (
	verifyAuthenticode func(string) error // or nil on non-Windows
	markTempFileFunc   func(string) error // or nil on non-Windows
)

func (up *updater) updateWindows() error {
	ver, err := requestedTailscaleVersion(updateArgs.version, up.track)
	if err != nil {
		return err
	}
	arch := runtime.GOARCH
	if arch == "386" {
		arch = "x86"
	}
	url := fmt.Sprintf("https://pkgs.tailscale.com/%s/tailscale-setup-%s-%s.msi", up.track, ver, arch)

	if up.currentOrDryRun(ver) {
		return nil
	}
	if !winutil.IsCurrentProcessElevated() {
		return errors.New("must be run as Administrator")
	}

	tsDir := filepath.Join(os.Getenv("ProgramData"), "Tailscale")
	msiDir := filepath.Join(tsDir, "MSICache")
	if fi, err := os.Stat(tsDir); err != nil {
		return fmt.Errorf("expected %s to exist, got stat error: %w", tsDir, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("expected %s to be a directory; got %v", tsDir, fi.Mode())
	}
	if err := os.MkdirAll(msiDir, 0700); err != nil {
		return err
	}

	if err := up.confirm(ver); err != nil {
		return err
	}
	msiTarget := filepath.Join(msiDir, path.Base(url))
	if err := downloadURLToFile(url, msiTarget); err != nil {
		return err
	}

	log.Printf("verifying MSI authenticode...")
	if err := verifyAuthenticode(msiTarget); err != nil {
		return fmt.Errorf("authenticode verification of %s failed: %w", msiTarget, err)
	}
	log.Printf("authenticode verification succeeded")

	log.Printf("making tailscale.exe copy to switch to...")
	selfCopy, err := makeSelfCopy()
	if err != nil {
		return err
	}
	defer os.Remove(selfCopy)
	log.Printf("running tailscale.exe copy for final install...")

	cmd := exec.Command(selfCopy, "update")
	cmd.Env = append(os.Environ(), winMSIEnv+"="+msiTarget)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Start(); err != nil {
		return err
	}
	// Once it's started, exit ourselves, so the binary is free
	// to be replaced.
	os.Exit(0)
	panic("unreachable")
}

func installMSI(msi string) error {
	var err error
	for tries := 0; tries < 2; tries++ {
		cmd := exec.Command("msiexec.exe", "/i", filepath.Base(msi), "/quiet", "/promptrestart", "/qn")
		cmd.Dir = filepath.Dir(msi)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		err = cmd.Run()
		if err == nil {
			break
		}
		uninstallVersion := version.Short()
		if v := os.Getenv("TS_DEBUG_UNINSTALL_VERSION"); v != "" {
			uninstallVersion = v
		}
		// Assume it's a downgrade, which msiexec won't permit. Uninstall our current version first.
		log.Printf("Uninstalling current version %q for downgrade...", uninstallVersion)
		cmd = exec.Command("msiexec.exe", "/x", msiUUIDForVersion(uninstallVersion), "/norestart", "/qn")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		err = cmd.Run()
		log.Printf("msiexec uninstall: %v", err)
	}
	return err
}

func msiUUIDForVersion(ver string) string {
	arch := runtime.GOARCH
	if arch == "386" {
		arch = "x86"
	}
	track := "unstable"
	if stable, ok := versionIsStable(ver); ok && stable {
		track = "stable"
	}
	msiURL := fmt.Sprintf("https://pkgs.tailscale.com/%s/tailscale-setup-%s-%s.msi", track, ver, arch)
	return "{" + strings.ToUpper(uuid.NewSHA1(uuid.NameSpaceURL, []byte(msiURL)).String()) + "}"
}

func makeSelfCopy() (tmpPathExe string, err error) {
	selfExe, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(selfExe)
	if err != nil {
		return "", err
	}
	defer f.Close()
	f2, err := os.CreateTemp("", "tailscale-updater-*.exe")
	if err != nil {
		return "", err
	}
	if f := markTempFileFunc; f != nil {
		if err := f(f2.Name()); err != nil {
			return "", err
		}
	}
	if _, err := io.Copy(f2, f); err != nil {
		f2.Close()
		return "", err
	}
	return f2.Name(), f2.Close()
}

func downloadURLToFile(urlSrc, fileDst string) (ret error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = tshttpproxy.ProxyFromEnvironment
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}

	quickCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	headReq := must.Get(http.NewRequestWithContext(quickCtx, "HEAD", urlSrc, nil))

	res, err := c.Do(headReq)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HEAD %s: %v", urlSrc, res.Status)
	}
	if res.ContentLength <= 0 {
		return fmt.Errorf("HEAD %s: unexpected Content-Length %v", urlSrc, res.ContentLength)
	}
	log.Printf("Download size: %v", res.ContentLength)

	hashReq := must.Get(http.NewRequestWithContext(quickCtx, "GET", urlSrc+".sha256", nil))
	hashRes, err := c.Do(hashReq)
	if err != nil {
		return err
	}
	hashHex, err := io.ReadAll(io.LimitReader(hashRes.Body, 100))
	hashRes.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s.sha256: %v", urlSrc, res.Status)
	}
	if err != nil {
		return err
	}
	wantHash, err := hex.DecodeString(string(strings.TrimSpace(string(hashHex))))
	if err != nil {
		return err
	}
	hash := sha256.New()

	dlReq := must.Get(http.NewRequestWithContext(context.Background(), "GET", urlSrc, nil))
	dlRes, err := c.Do(dlReq)
	if err != nil {
		return err
	}
	// TODO(bradfitz): resume from existing partial file on disk
	if dlRes.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %v", urlSrc, dlRes.Status)
	}

	of, err := os.Create(fileDst)
	if err != nil {
		return err
	}
	defer func() {
		if ret != nil {
			of.Close()
			// TODO(bradfitz): os.Remove(fileDst) too? or keep it to resume from/debug later.
		}
	}()
	pw := &progressWriter{total: res.ContentLength}
	n, err := io.Copy(io.MultiWriter(hash, of, pw), io.LimitReader(dlRes.Body, res.ContentLength))
	if err != nil {
		return err
	}
	if n != res.ContentLength {
		return fmt.Errorf("downloaded %v; want %v", n, res.ContentLength)
	}
	if err := of.Close(); err != nil {
		return err
	}
	pw.print()

	if !bytes.Equal(hash.Sum(nil), wantHash) {
		return fmt.Errorf("SHA-256 of downloaded MSI didn't match expected value")
	}
	log.Printf("hash matched")

	return nil
}

type progressWriter struct {
	done      int64
	total     int64
	lastPrint time.Time
}

func (pw *progressWriter) Write(p []byte) (n int, err error) {
	pw.done += int64(len(p))
	if time.Since(pw.lastPrint) > 2*time.Second {
		pw.print()
	}
	return len(p), nil
}

func (pw *progressWriter) print() {
	pw.lastPrint = time.Now()
	log.Printf("Downloaded %v/%v (%.1f%%)", pw.done, pw.total, float64(pw.done)/float64(pw.total)*100)
}

func haveExecutable(name string) bool {
	path, err := exec.LookPath(name)
	return err == nil && path != ""
}

func requestedTailscaleVersion(ver, track string) (string, error) {
	if ver != "" {
		return ver, nil
	}
	return latestTailscaleVersion(track)
}

func latestTailscaleVersion(track string) (string, error) {
	url := fmt.Sprintf("https://pkgs.tailscale.com/%s/?mode=json&os=%s", track, runtime.GOOS)
	res, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetching latest tailscale version: %w", err)
	}
	var latest struct {
		Version string
	}
	err = json.NewDecoder(res.Body).Decode(&latest)
	res.Body.Close()
	if err != nil {
		return "", fmt.Errorf("decoding JSON: %v: %w", res.Status, err)
	}
	if latest.Version == "" {
		return "", fmt.Errorf("no version found at %q", url)
	}
	return latest.Version, nil
}

func requireRoot() error {
	if os.Geteuid() == 0 {
		return nil
	}
	switch runtime.GOOS {
	case "linux":
		return errors.New("must be root; use sudo")
	case "freebsd", "openbsd":
		return errors.New("must be root; use doas")
	default:
		return errors.New("must be root")
	}
}
