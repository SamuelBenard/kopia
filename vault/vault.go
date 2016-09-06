package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/storage"

	"golang.org/x/crypto/hkdf"
)

const (
	formatBlockID           = "format"
	repositoryConfigBlockID = "repo"

	colocatedVaultItemPrefix = "VLT"
)

var (
	purposeAESKey         = []byte("AES")
	purposeChecksumSecret = []byte("CHECKSUM")
)

// ErrItemNotFound is an error returned when a vault item cannot be found.
var ErrItemNotFound = errors.New("item not found")

// Vault is a secure storage for the secrets.
type Vault struct {
	Storage    storage.Storage
	Format     Format
	RepoConfig RepositoryConfig

	masterKey  []byte
	itemPrefix string
}

// Put saves the specified content in a vault under a specified name.
func (v *Vault) Put(itemID string, content []byte) error {
	if err := checkReservedName(itemID); err != nil {
		return err
	}

	return v.writeEncryptedBlock(itemID, content)
}

func (v *Vault) writeEncryptedBlock(itemID string, content []byte) error {
	blk, err := v.newCipher()
	if err != nil {
		return err
	}

	if blk != nil {
		hash, err := v.newChecksum()
		if err != nil {
			return err
		}

		ivLength := blk.BlockSize()
		ivPlusContentLength := ivLength + len(content)
		cipherText := make([]byte, ivPlusContentLength+hash.Size())

		// Store IV at the beginning of ciphertext.
		iv := cipherText[0:ivLength]
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			return err
		}

		ctr := cipher.NewCTR(blk, iv)
		ctr.XORKeyStream(cipherText[ivLength:], content)
		hash.Write(cipherText[0:ivPlusContentLength])
		copy(cipherText[ivPlusContentLength:], hash.Sum(nil))

		content = cipherText
	}

	return v.Storage.PutBlock(v.itemPrefix+itemID, content, storage.PutOptionsOverwrite)
}

func (v *Vault) readEncryptedBlock(itemID string) ([]byte, error) {
	content, err := v.Storage.GetBlock(v.itemPrefix + itemID)
	if err != nil {
		if err == storage.ErrBlockNotFound {
			return nil, ErrItemNotFound
		}
		return nil, fmt.Errorf("unexpected error reading %v: %v", itemID, err)
	}

	return v.decryptBlock(content)
}

func (v *Vault) decryptBlock(content []byte) ([]byte, error) {
	blk, err := v.newCipher()
	if err != nil {
		return nil, err
	}

	if blk != nil {
		hash, err := v.newChecksum()
		if err != nil {
			return nil, err
		}

		p := len(content) - hash.Size()
		hash.Write(content[0:p])
		expectedChecksum := hash.Sum(nil)
		actualChecksum := content[p:]

		if !hmac.Equal(expectedChecksum, actualChecksum) {
			return nil, fmt.Errorf("cannot read encrypted block: incorrect checksum")
		}

		ivLength := blk.BlockSize()

		plainText := make([]byte, len(content)-ivLength-hash.Size())
		iv := content[0:blk.BlockSize()]

		ctr := cipher.NewCTR(blk, iv)
		ctr.XORKeyStream(plainText, content[ivLength:len(content)-hash.Size()])

		content = plainText
	}

	return content, nil
}

func (v *Vault) newChecksum() (hash.Hash, error) {
	switch v.Format.Checksum {
	case "hmac-sha-256":
		key := make([]byte, 32)
		v.deriveKey(purposeChecksumSecret, key)
		return hmac.New(sha256.New, key), nil

	default:
		return nil, fmt.Errorf("unsupported checksum format: %v", v.Format.Checksum)
	}

}

func (v *Vault) newCipher() (cipher.Block, error) {
	switch v.Format.Encryption {
	case "none":
		return nil, nil
	case "aes-128":
		k := make([]byte, 16)
		v.deriveKey(purposeAESKey, k)
		return aes.NewCipher(k)
	case "aes-192":
		k := make([]byte, 24)
		v.deriveKey(purposeAESKey, k)
		return aes.NewCipher(k)
	case "aes-256":
		k := make([]byte, 32)
		v.deriveKey(purposeAESKey, k)
		return aes.NewCipher(k)
	default:
		return nil, fmt.Errorf("unsupported encryption format: %v", v.Format.Encryption)
	}

}

func (v *Vault) deriveKey(purpose []byte, key []byte) error {
	k := hkdf.New(sha256.New, v.masterKey, v.Format.UniqueID, purpose)
	_, err := io.ReadFull(k, key)
	return err
}

// Get returns the contents of a specified vault item.
func (v *Vault) Get(itemID string) ([]byte, error) {
	if err := checkReservedName(itemID); err != nil {
		return nil, err
	}

	return v.readEncryptedBlock(itemID)
}

func (v *Vault) getJSON(itemID string, content interface{}) error {
	j, err := v.readEncryptedBlock(itemID)
	if err != nil {
		return err
	}

	return json.Unmarshal(j, content)
}

// Put stores the contents of an item stored in a vault with a given ID.
func (v *Vault) putJSON(id string, content interface{}) error {
	j, err := json.Marshal(content)
	if err != nil {
		return err
	}

	return v.writeEncryptedBlock(id, j)
}

// List returns the list of vault items matching the specified prefix.
func (v *Vault) List(prefix string) ([]string, error) {
	var result []string

	for b := range v.Storage.ListBlocks(v.itemPrefix + prefix) {
		if b.Error != nil {
			return result, b.Error
		}

		itemID := strings.TrimPrefix(b.BlockID, v.itemPrefix)
		if !isReservedName(itemID) {
			result = append(result, itemID)
		}
	}
	return result, nil
}

// Close releases any resources held by Vault and closes repository connection.
func (v *Vault) Close() error {
	return v.Storage.Close()
}

// Config represents JSON-compatible configuration of the vault connection, including vault key.
type Config struct {
	ConnectionInfo storage.ConnectionInfo `json:"connection"`
	Key            []byte                 `json:"key,omitempty"`
}

// Config returns a configuration of vault storage its credentials that's suitable
// for storing in configuration file.
func (v *Vault) Config() (*Config, error) {
	cip, ok := v.Storage.(storage.ConnectionInfoProvider)
	if !ok {
		return nil, errors.New("repository does not support persisting configuration")
	}

	ci := cip.ConnectionInfo()

	return &Config{
		ConnectionInfo: ci,
		Key:            v.masterKey,
	}, nil
}

// Remove deletes the specified vault item.
func (v *Vault) Remove(itemID string) error {
	if err := checkReservedName(itemID); err != nil {
		return err
	}

	return v.Storage.DeleteBlock(v.itemPrefix + itemID)
}

// Create initializes a Vault attached to the specified repository.
func Create(
	vaultStorage storage.Storage,
	vaultFormat *Format,
	vaultCreds Credentials,
	repoStorage storage.Storage,
	repoFormat *repo.Format,
) (*Vault, error) {
	v := Vault{
		Storage: vaultStorage,
		Format:  *vaultFormat,
	}

	if repoStorage == nil {
		repoStorage = vaultStorage
		v.itemPrefix = colocatedVaultItemPrefix
	}

	cip, ok := repoStorage.(storage.ConnectionInfoProvider)
	if !ok {
		return nil, errors.New("repository does not support persisting configuration")
	}

	v.Format.Version = "1"
	v.Format.UniqueID = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, v.Format.UniqueID); err != nil {
		return nil, err
	}
	v.masterKey = vaultCreds.getMasterKey(v.Format.UniqueID)

	formatBytes, err := json.Marshal(&v.Format)
	if err != nil {
		return nil, err
	}

	if err := vaultStorage.PutBlock(v.itemPrefix+formatBlockID, formatBytes, storage.PutOptionsOverwrite); err != nil {
		return nil, err
	}

	// Write encrypted repository configuration block.
	rc := RepositoryConfig{
		Format: repoFormat,
	}

	if repoStorage != vaultStorage {
		ci := cip.ConnectionInfo()
		rc.Connection = &ci
	}

	if err := v.putJSON(repositoryConfigBlockID, &rc); err != nil {
		return nil, err
	}

	v.RepoConfig = rc
	return &v, nil
}

// CreateColocated initializes a Vault attached to a Repository sharing the same storage.
func CreateColocated(
	sharedStorage storage.Storage,
	vaultFormat *Format,
	vaultCreds Credentials,
	repoFormat *repo.Format,
) (*Vault, error) {
	return Create(sharedStorage, vaultFormat, vaultCreds, nil, repoFormat)
}

// RepositoryConfig stores the configuration of the repository associated with the vault.
type RepositoryConfig struct {
	Connection *storage.ConnectionInfo `json:"connection"`
	Format     *repo.Format            `json:"format"`
}

// Open opens a vault.
func Open(vaultStorage storage.Storage, vaultCreds Credentials) (*Vault, error) {
	v := Vault{
		Storage: vaultStorage,
	}

	var prefix string
	var wg sync.WaitGroup

	var blocks [4][]byte

	f := func(index int, name string) {
		blocks[index], _ = vaultStorage.GetBlock(name)
		wg.Done()
	}

	wg.Add(4)
	go f(0, formatBlockID)
	go f(1, repositoryConfigBlockID)
	go f(2, colocatedVaultItemPrefix+formatBlockID)
	go f(3, colocatedVaultItemPrefix+repositoryConfigBlockID)
	wg.Wait()

	if blocks[0] == nil && blocks[2] == nil {
		return nil, fmt.Errorf("vault format block not found")
	}

	var offset = 0
	if blocks[0] == nil {
		prefix = colocatedVaultItemPrefix
		offset = 2
	}

	err := json.Unmarshal(blocks[offset], &v.Format)
	if err != nil {
		return nil, err
	}

	v.masterKey = vaultCreds.getMasterKey(v.Format.UniqueID)
	v.itemPrefix = prefix

	cfgData, err := v.decryptBlock(blocks[offset+1])
	if err != nil {
		return nil, err
	}

	var rc RepositoryConfig

	if err := json.Unmarshal(cfgData, &rc); err != nil {
		return nil, err
	}

	v.RepoConfig = rc

	return &v, nil
}

func isReservedName(itemID string) bool {
	switch itemID {
	case formatBlockID, repositoryConfigBlockID:
		return true

	default:
		return false
	}
}

func checkReservedName(itemID string) error {
	if isReservedName(itemID) {
		return fmt.Errorf("invalid vault item name: '%v'", itemID)
	}

	return nil
}
