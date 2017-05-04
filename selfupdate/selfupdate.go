// Update protocol:
//
//   GET hk.heroku.com/hk/linux-amd64.json
//
//   200 ok
//   {
//       "Version": "2",
//       "Sha256": "..." // base64
//   }
//
// then
//
//   GET hkpatch.s3.amazonaws.com/hk/1/2/linux-amd64
//
//   200 ok
//   [bsdiff data]
//
// or
//
//   GET hkdist.s3.amazonaws.com/hk/2/linux-amd64.gz
//
//   200 ok
//   [gzipped executable data]
//
//
package selfupdate

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/kardianos/osext"
	"github.com/kr/binarydist"
	"github.com/pkg/errors"
	"gopkg.in/inconshreveable/go-update.v0"
)

const (
	upcktimePath = "cktime"
	plat         = runtime.GOOS + "-" + runtime.GOARCH
	devValidTime = 7 * 24 * time.Hour
)

var ErrHashMismatch = errors.New("new file hash mismatch after patch")
var up = update.New()
var defaultHTTPRequester = HTTPRequester{}

// Updater is the configuration and runtime data for doing an update.
//
// Note that ApiURL, BinURL and DiffURL should have the same value if all files are available at the same location.
//
// Example:
//
//  updater := &selfupdate.Updater{
//  	CurrentVersion: version,
//  	ApiURL:         "http://updates.yourdomain.com/",
//  	BinURL:         "http://updates.yourdownmain.com/",
//  	DiffURL:        "http://updates.yourdomain.com/",
//  	Dir:            "update/",
//  	CmdName:        "myapp", // app name
//  }
//  if updater != nil {
//  	go updater.BackgroundRun()
//  }
type Updater struct {
	CurrentVersion string    // Currently running version.
	ApiURL         string    // Base URL for API requests (json files).
	CmdName        string    // Command name is appended to the ApiURL like http://apiurl/CmdName/. This represents one binary.
	BinURL         string    // Base URL for full binary downloads.
	DiffURL        string    // Base URL for diff downloads.
	Dir            string    // Directory to store selfupdate state.
	ForceCheck     bool      // Check for update regardless of cktime timestamp
	Requester      Requester //Optional parameter to override existing http request handler
	Info           struct {
		Version string
		Sha256  []byte
	}
}

// getExecRelativeDir relativizes the directory to store selfupdate state
// from the executable directory.
func (u *Updater) getExecRelativeDir(dir string) (string, error) {
	filename, err := osext.Executable()
	if err != nil {
		return "", errors.Wrapf(err,
			"Couldn't get path to self executable")
	}

	path := filepath.Join(filepath.Dir(filename), dir)

	log.
		WithField("executable", filename).
		WithField("relative-path", path).
		Debug("Directory to store selfupdate state")

	return path, nil
}

// BackgroundRun starts the update check and apply cycle.
func (u *Updater) BackgroundRun() error {
	dir, err := u.getExecRelativeDir(u.Dir)
	if err != nil {
		return errors.Wrapf(err,
			"Couldn't get directory relative to executable for updates")
	}

	if err := os.MkdirAll(dir, 0777); err != nil {
		return errors.Wrapf(err,
			"Couldn't create directory for storing updates (dir=%s)",
			dir)
	}
	if u.wantUpdate() {
		if err := up.CanUpdate(); err != nil {
			return errors.Wrapf(err,
				"Wants to update but can't")
		}
		if err := u.update(); err != nil {
			return errors.Wrapf(err,
				"Failed performing update even though it can")
		}
	}
	return nil
}

func (u *Updater) wantUpdate() bool {
	path, err := u.getExecRelativeDir(u.Dir + upcktimePath)
	if err != nil {
		log.Error(err)
		return false
	}
	if u.CurrentVersion == "dev" || (!u.ForceCheck && readTime(path).After(time.Now())) {
		return false
	}
	wait := 24*time.Hour + randDuration(24*time.Hour)
	return writeTime(path, time.Now().Add(wait))
}

// update performs the actual update of the executable
func (u *Updater) update() error {
	path, err := osext.Executable()
	if err != nil {
		return errors.Wrapf(err,
			"Couldn't get path to executable (self)")
	}
	old, err := os.Open(path)
	if err != nil {
		return errors.Wrapf(err,
			"Couldn't open self executable")
	}
	defer old.Close()

	err = u.fetchInfo()
	if err != nil {
		return errors.Wrapf(err,
			"Couldn't properly fetch JSON information for updates")
	}
	if u.Info.Version == u.CurrentVersion {
		log.Debug("Already at latest version :)")
		return nil
	}
	bin, err := u.fetchAndVerifyPatch(old)
	if err != nil {
		if err == ErrHashMismatch {
			log.Debug("update: hash mismatch from patched binary")
		} else {
			if u.DiffURL != "" {
				log.WithError(err).Debug("update: patching binary")
			}
		}

		bin, err = u.fetchAndVerifyFullBin()
		if err != nil {
			if err == ErrHashMismatch {
				log.Debug("update: hash mismatch from full binary")
			} else {
				log.WithError(err).Debug("update: fetching full binary")
			}
			return err
		}
	}

	// close the old binary before installing because on windows
	// it can't be renamed if a handle to the file is still open
	old.Close()

	err, errRecover := up.FromStream(bytes.NewBuffer(bin))
	if errRecover != nil {
		return fmt.Errorf("update and recovery errors: %q %q", err, errRecover)
	}
	if err != nil {
		return err
	}
	return nil
}

// fetchInfo gets the `json` file containing update information
func (u *Updater) fetchInfo() error {
	var fullUrl = u.ApiURL + url.QueryEscape(u.CmdName) + "/" + url.QueryEscape(plat) + ".json"
	r, err := u.fetch(fullUrl)
	if err != nil {
		return errors.Wrapf(err,
			"Couldn't fetch `json` with information for update (url=%s)",
			fullUrl)
	}
	defer r.Close()
	err = json.NewDecoder(r).Decode(&u.Info)
	if err != nil {
		return errors.Wrapf(err,
			"Couldn't decode JSON (%s)")
	}
	if len(u.Info.Sha256) != sha256.Size {
		return errors.Errorf(
			"Bad cmd hash in JSON info")
	}
	return nil
}

func (u *Updater) fetchAndVerifyPatch(old io.Reader) ([]byte, error) {
	bin, err := u.fetchAndApplyPatch(old)
	if err != nil {
		return nil, err
	}
	if !verifySha(bin, u.Info.Sha256) {
		return nil, ErrHashMismatch
	}
	return bin, nil
}

func (u *Updater) fetchAndApplyPatch(old io.Reader) ([]byte, error) {
	var argCmdName = url.QueryEscape(u.CmdName)
	var argCurrentVersion = url.QueryEscape(u.CurrentVersion)
	var argInfoVersion = url.QueryEscape(u.Info.Version)
	var argPlatform = url.QueryEscape(plat)
	var patchUrl = u.DiffURL + fmt.Sprintf("%s/%s/%s",
		argCmdName, argCurrentVersion, argInfoVersion, argPlatform)

	log.WithField("patch-url", patchUrl).Debug("Starting to fetch patch")

	r, err := u.fetch(patchUrl)
	if err != nil {
		return nil, errors.Wrapf(err,
			"Errored fetching path (url=%s)",
			patchUrl)
	}
	defer r.Close()
	var buf bytes.Buffer

	err = binarydist.Patch(old, &buf, r)
	if err != nil {
		return nil, errors.Wrapf(err,
			"Errored using binarydist to patch (url=%s)",
			patchUrl)
	}

	return buf.Bytes(), nil
}

func (u *Updater) fetchAndVerifyFullBin() ([]byte, error) {
	bin, err := u.fetchBin()
	if err != nil {
		return nil, errors.Wrapf(err,
			"Errored fetching full binary")
	}
	verified := verifySha(bin, u.Info.Sha256)
	if !verified {
		return nil, errors.Wrapf(ErrHashMismatch,
			"Hash mismatch")
	}
	return bin, nil
}

func (u *Updater) fetchBin() ([]byte, error) {
	var argCmdName = url.QueryEscape(u.CmdName)
	var argInfoVersion = url.QueryEscape(u.Info.Version)
	var argPlatform = url.QueryEscape(plat)
	var fetchUrl = u.BinURL + fmt.Sprintf("%s/%s/%s.gz",
		argCmdName, argInfoVersion, argPlatform)

	log.WithField("url", fetchUrl).Debug("Starting to fetch full binary")

	r, err := u.fetch(fetchUrl)
	if err != nil {
		return nil, errors.Wrapf(err,
			"Failed to fetch full binary (url=%s)",
			fetchUrl)
	}
	defer r.Close()
	buf := new(bytes.Buffer)

	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, errors.Wrapf(err,
			"Failed to create gzip reader")
	}
	if _, err = io.Copy(buf, gz); err != nil {
		return nil, errors.Wrapf(err,
			"Failed to copy gzip content to buf")
	}

	return buf.Bytes(), nil
}

// returns a random duration in [0,n).
func randDuration(n time.Duration) time.Duration {
	return time.Duration(rand.Int63n(int64(n)))
}

func (u *Updater) fetch(url string) (io.ReadCloser, error) {
	if u.Requester == nil {
		return defaultHTTPRequester.Fetch(url)
	}

	readCloser, err := u.Requester.Fetch(url)
	if err != nil {
		return nil, err
	}

	if readCloser == nil {
		return nil, fmt.Errorf(
			"Fetch was expected to return non-nil ReadCloser")
	}

	return readCloser, nil
}

func readTime(path string) time.Time {
	p, err := ioutil.ReadFile(path)
	if os.IsNotExist(err) {
		return time.Time{}
	}
	if err != nil {
		return time.Now().Add(1000 * time.Hour)
	}
	t, err := time.Parse(time.RFC3339, string(p))
	if err != nil {
		return time.Now().Add(1000 * time.Hour)
	}
	return t
}

func verifySha(bin []byte, sha []byte) bool {
	h := sha256.New()
	h.Write(bin)
	return bytes.Equal(h.Sum(nil), sha)
}

func writeTime(path string, t time.Time) bool {
	return ioutil.WriteFile(
		path,
		[]byte(t.Format(time.RFC3339)),
		0644) == nil
}
