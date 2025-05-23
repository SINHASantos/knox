package client

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"

	"gopkg.in/fsnotify.v1"

	"github.com/pinterest/knox"
)

var cmdDaemon = &Command{
	Run:       runDaemon,
	UsageLine: "daemon",
	Short:     "runs a process to keep keys in sync with server",
	Long: `
daemon runs the knox process that will keep keys in sync.

This process will keep running until sent a kill signal or it crashes.

This maintains a file system cache of knox keys that is used for all other knox commands.

For more about knox, see https://github.com/pinterest/knox.

See also: knox register, knox unregister
	`,
}

var daemonFolder = "/var/lib/knox"
var daemonToRegister = "/.registered"
var daemonKeys = "/v0/keys/"

var lockTimeout = 10 * time.Second
var lockRetryTime = 50 * time.Millisecond

var defaultFilePermission os.FileMode = 0666
var defaultDirPermission os.FileMode = 0777

var daemonRefreshTime = 10 * time.Minute

const tinkPrefix = "tink:"

func runDaemon(cmd *Command, args []string) *ErrorStatus {

	if os.Getenv("KNOX_MACHINE_AUTH") == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return &ErrorStatus{fmt.Errorf("You're on a host with no name: %s", err.Error()), false}
		}
		os.Setenv("KNOX_MACHINE_AUTH", hostname)
	}

	d := daemon{
		dir:          daemonFolder,
		registerFile: daemonToRegister,
		keysDir:      daemonKeys,
		cli:          cli,
	}
	err := d.initialize()
	if err != nil {
		return &ErrorStatus{err, false}
	}
	d.loop(daemonRefreshTime)
	return nil
}

type daemon struct {
	dir             string
	registerFile    string
	registerKeyFile Keys
	keysDir         string
	cli             knox.APIClient
	updateErrCount  uint64
	getKeyErrCount  uint64
	successCount    uint64
}

func (d *daemon) loop(refresh time.Duration) {
	t := time.NewTicker(refresh)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fatalf("Unable to watch files: %s", err.Error())
	}
	watcher.Add(d.registerFilename())

	for {
		logf("Daemon updating all registered keys")
		start := time.Now()
		err := d.update()
		if err != nil {
			d.updateErrCount++
			logf("Failed to update keys: %s", err.Error())
		} else {
			d.successCount++
		}
		logf("Update of keys completed after %d ms", time.Since(start).Milliseconds())

		select {
		case event := <-watcher.Events:
			// On any change to register file
			logf("Got file watcher event: %s on %s", event.Op.String(), event.Name)
		case <-t.C:
			// add random jitter to prevent a stampede
			<-time.After(time.Duration(rand.Intn(10)) * time.Millisecond)
			daemonReportMetrics(map[string]uint64{
				"err":     d.updateErrCount,
				"get_err": d.getKeyErrCount,
				"success": d.successCount,
			})
		}
	}
}

func (d *daemon) initialize() error {
	err := os.MkdirAll(d.dir, defaultDirPermission)
	if err != nil {
		return fmt.Errorf("Failed to initialize /var/lib/knox (run 'sudo mkdir /var/lib/knox'?): %s", err.Error())
	}

	// Need to chmod due to a umask set on masterless puppet machines
	err = os.Chmod(d.dir, defaultDirPermission)
	if err != nil {
		return fmt.Errorf("Failed to open up directory permissions: %s", err.Error())
	}
	err = os.MkdirAll(d.keyDir(), defaultDirPermission)
	if err != nil {
		return fmt.Errorf("Failed to make key folders: %s", err.Error())
	}

	// Need to chmod due to a umask set on masterless puppet machines
	err = os.Chmod(d.keyDir(), defaultDirPermission)
	if err != nil {
		return fmt.Errorf("Failed to open up directory permissions: %s", err.Error())
	}
	_, err = os.Stat(d.registerFilename())
	if os.IsNotExist(err) {
		err := os.WriteFile(d.registerFilename(), []byte{}, defaultFilePermission)
		if err != nil {
			return fmt.Errorf("Failed to initialize registered key file: %s", err.Error())
		}
	} else if err != nil {
		return err
	}

	// Need to chmod due to a umask set on masterless puppet machines
	err = os.Chmod(d.registerFilename(), defaultFilePermission)
	if err != nil {
		return fmt.Errorf("Failed to open up register file permissions: %s", err.Error())
	}
	d.registerKeyFile = NewKeysFile(d.registerFilename())
	return nil
}

func (d *daemon) update() error {
	err := d.registerKeyFile.Lock()
	if err != nil {
		return err
	}
	// defer this so that functions can update the register file.
	defer d.registerKeyFile.Unlock()
	keyIDs, err := d.registerKeyFile.Get()
	if err != nil {
		return err
	}
	logf("Requested keys: %s", keyIDs)

	keyMap := map[string]string{}
	existingKeys := map[string]bool{}
	for _, k := range keyIDs {
		// set default value to empty string
		keyMap[k] = ""
		existingKeys[k] = false
	}

	currentKeyIDs, err := d.currentRegisteredKeys()
	if err != nil {
		return err
	}
	logf("Current keys on disk: %s", currentKeyIDs)

	for _, keyID := range currentKeyIDs {
		existingKeys[keyID] = true

		if _, present := keyMap[keyID]; present {
			key, err := d.cli.CacheGetKey(keyID)
			if err != nil {
				// Keep going in spite of failure
				logf("error getting cache key: %s", err)
				// Remove existing cached key with invalid format (saved with previous version clients)
				if _, err = os.Stat(d.keyFilename(keyID)); err == nil {
					d.deleteKey(keyID)
				}
			} else {
				keyMap[keyID] = key.VersionHash
			}
		} else {
			d.deleteKey(keyID)
		}
	}

	if len(keyMap) > 0 {
		updatedKeys, err := d.cli.GetKeys(keyMap)
		if err != nil {
			return err
		}
		logf("Updated keys received from server: %s", updatedKeys)
		for _, k := range updatedKeys {
			err = d.processKey(k)
			existingKeys[k] = true

			if err != nil {
				// Keep going in spite of failure
				d.getKeyErrCount++
				logf("error processing key: %s", err)
			}
		}
	}
	// Find out if we missed anything (useful for humans reading the logs)
	// If key was not processed, and is also not current, then it didn't exist
	notFound := []string{}
	for id, exists := range existingKeys {
		if !exists {
			notFound = append(notFound, id)
		}
	}
	logf("Keys not found on server: %s", notFound)

	return nil
}

func (d daemon) deleteKey(keyID string) error {
	return os.Remove(d.keyFilename(keyID))
}

func (d daemon) currentRegisteredKeys() ([]string, error) {
	files, err := os.ReadDir(d.keyDir())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		out = append(out, f.Name())
	}
	return out, nil
}

func (d daemon) keyDir() string {
	return path.Join(d.dir, d.keysDir)
}

func (d daemon) registerFilename() string {
	return path.Join(d.dir, d.registerFile)
}

func (d daemon) keyFilename(id string) string {
	return path.Join(d.dir, d.keysDir, id)
}

func (d daemon) processKey(keyID string) error {
	key, err := d.cli.NetworkGetKey(keyID)
	if err != nil {
		if err.Error() == "User or machine not authorized" || err.Error() == "Key identifer does not exist" {
			// This removes keys that do not exist or the machine is unauthorized to access
			d.registerKeyFile.Remove([]string{keyID})
		}
		return fmt.Errorf("Error getting key %s: %s", keyID, err.Error())
	}
	// Do not cache any new keys if they have invalid content
	if key.ID == "" || key.ACL == nil || key.VersionList == nil || key.VersionHash == "" {
		return fmt.Errorf("invalid key content returned")
	}

	if strings.HasPrefix(keyID, tinkPrefix) {
		keysetHandle, _, err := getTinkKeysetHandleFromKnoxVersionList(key.VersionList)
		if err != nil {
			return fmt.Errorf("Error fetching keyset handle for this tink key %s: %s", keyID, err.Error())
		}
		tinkKeyset, err := convertTinkKeysetHandleToBytes(keysetHandle)
		if err != nil {
			return fmt.Errorf("Error converting tink keyset handle to bytes %s: %s", keyID, err.Error())
		}
		key.TinkKeyset = base64.StdEncoding.EncodeToString(tinkKeyset)
	}

	b, err := json.Marshal(key)
	if err != nil {
		return fmt.Errorf("Error marshalling key %s: %s", keyID, err.Error())
	}
	// Write to tmpfile, mv to normal location. Close + rm on failures
	tmpFile, err := os.CreateTemp(d.dir, fmt.Sprintf(".*.%s.tmp", keyID))
	if err != nil {
		return fmt.Errorf("Error opening tmp file for key %s: %s", keyID, err.Error())
	}
	_, err = tmpFile.Write(b)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return fmt.Errorf("Error writing key %s to file: %s", keyID, err.Error())
	}
	// Done writing
	tmpFile.Close()

	err = os.Rename(tmpFile.Name(), d.keyFilename(keyID))
	if err != nil {
		os.Remove(tmpFile.Name())
		return fmt.Errorf("Error renaming key %s temporary file: %s", keyID, err.Error())
	}

	err = os.Chmod(d.keyFilename(keyID), defaultFilePermission)
	if err != nil {
		return fmt.Errorf("Failed to open up key file permissions: %s", err.Error())
	}
	return nil
}

// Keys are an interface for storing a list of key ids (for use with the register file to provide locks)
type Keys interface {
	Get() ([]string, error)
	Add([]string) error
	Overwrite([]string) error
	Remove([]string) error
	Lock() error
	Unlock() error
}

// KeysFile is an implementation of Keys based on the file system for the register file.
type KeysFile struct {
	fn string
	*flock
}

// NewKeysFile takes in a filename and outputs an implementation of the Keys interface
func NewKeysFile(fn string) Keys {
	return &KeysFile{fn, newFlock()}
}

// Lock performs the nonblocking syscall lock and retries until the global timeout is met.
func (k *KeysFile) Lock() error {
	err := k.lock(k, defaultFilePermission, true, lockTimeout)

	// Timeout means someone else is using our lock, which is unusual.
	// Let's collect some extra debugging information to find out why.
	if err == ErrTimeout && runtime.GOOS == "linux" {
		lockHolders, err := identifyLockHolders(k.fn)
		if err != nil {
			logf("hit timeout, found lock holder information:\n%s", lockHolders)
		}
	}

	// Annotate error with path to file to make debugging easier
	if err != nil {
		return fmt.Errorf("unable to obtain lock on file '%s': %s", k.fn, err.Error())
	}
	return nil
}

// Unlock performs the nonblocking syscall unlock and retries until the global timeout is met.
func (k *KeysFile) Unlock() error {
	err := k.unlock(k)

	// Annotate error with path to file to make debugging easier
	if err != nil {
		return fmt.Errorf("unable to release lock on file '%s': %s", k.fn, err.Error())
	}
	return nil
}

// Get will get the list of key ids. It expects Lock to have been called.
func (k *KeysFile) Get() ([]string, error) {
	b, err := os.ReadFile(k.fn)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(b)), nil
}

// Remove will remove the input key ids from the list. It expects Lock to have been called.
func (k *KeysFile) Remove(ks []string) error {
	oldKeys, err := k.Get()
	if err != nil {
		if os.IsNotExist(err) {
			oldKeys = []string{}
		} else {
			return err
		}
	}
	// Use a map to remove any duplicates
	newKeys := make(map[string]bool)
	for _, oldK := range oldKeys {
		removeIt := false
		for _, k := range ks {
			if k == oldK {
				removeIt = true
				break
			}
		}
		if !removeIt {
			newKeys[oldK] = true
		}
	}

	var buffer bytes.Buffer
	for k := range newKeys {
		buffer.WriteString(k)
		buffer.WriteByte('\n')
	}
	return os.WriteFile(k.fn, buffer.Bytes(), 0666)
}

// Add will add the key IDs to the list. It expects Lock to have been called.
func (k *KeysFile) Add(ks []string) error {
	oldKeys, err := k.Get()
	if err != nil {
		if os.IsNotExist(err) {
			oldKeys = []string{}
		} else {
			return err
		}
	}
	// Use a map to remove any duplicates
	newKeys := make(map[string]bool)
	for _, k := range oldKeys {
		newKeys[k] = true
	}
	for _, k := range ks {
		newKeys[k] = true
	}
	if len(newKeys) == len(oldKeys) {
		// Do not write if there are no changes
		return nil
	}

	var buffer bytes.Buffer
	for k := range newKeys {
		buffer.WriteString(k)
		buffer.WriteByte('\n')
	}
	return os.WriteFile(k.fn, buffer.Bytes(), 0666)
}

// Overwrite deletes all existing values in the key list and writes the input.
// It expects Lock to have been called.
func (k *KeysFile) Overwrite(ks []string) error {
	// Use a map to remove any duplicates
	newKeys := make(map[string]bool)
	for _, k := range ks {
		newKeys[k] = true
	}

	var buffer bytes.Buffer
	for k := range newKeys {
		buffer.WriteString(k)
		buffer.WriteByte('\n')
	}
	return os.WriteFile(k.fn, buffer.Bytes(), 0666)
}

func identifyLockHolders(filename string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("error identifying lock holder: works only on linux")
	}

	cmd := exec.Command("lsof", filename)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("error identifying lock holder: %s", err.Error())
	}

	return string(out), nil
}
