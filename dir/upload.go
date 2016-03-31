package dir

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/kopia/kopia/cas"
	"github.com/kopia/kopia/content"
)

var ErrUploadCancelled = errors.New("upload cancelled")

// Uploader supports efficient uploading files and directories to CAS storage.
type Uploader interface {
	UploadFile(path string) (content.ObjectID, error)
	UploadDir(path string, previousObjectID content.ObjectID) (content.ObjectID, error)
	Cancel()
}

type uploader struct {
	mgr    cas.ObjectManager
	lister Lister

	cancelled int32
}

func (u *uploader) isCancelled() bool {
	return atomic.LoadInt32(&u.cancelled) != 0
}

func (u *uploader) UploadFile(path string) (content.ObjectID, error) {
	file, err := os.Open(path)
	if err != nil {
		return content.NullObjectID, fmt.Errorf("unable to open file %s: %v", path, err)
	}
	defer file.Close()

	writer := u.mgr.NewWriter(
		cas.WithDescription("FILE:"+path),
		cas.WithBlockNamePrefix("F"),
	)
	defer writer.Close()

	io.Copy(writer, file)
	result, err := writer.Result(false)
	if err != nil {
		return content.NullObjectID, err
	}

	return result, nil
}

func (u *uploader) UploadDir(path string, previous content.ObjectID) (content.ObjectID, error) {
	if u.isCancelled() {
		return previous, ErrUploadCancelled
	}

	listing, err := u.lister.List(path)
	if err != nil {
		return content.NullObjectID, err
	}

	var cached Listing

	if previous != "" {
		if r, err := u.mgr.Open(previous); err == nil {
			cached, err = ReadDir(r)
			if err != nil {
				log.Printf("WARNING: unable to cached read directory: %v", err)
			}
		}
	}

	directoryMatchesCache := len(cached.Entries) == len(listing.Entries)
	for _, e := range listing.Entries {
		fullPath := filepath.Join(path, e.Name)

		// See if we had this name during previous pass.
		cachedEntry := cached.FindEntryName(e.Name)

		// ... and whether file metadata is identical to the previous one.
		cachedMetadataMatches := e.metadataEquals(cachedEntry)

		// If not, directoryMatchesCache becomes false.
		directoryMatchesCache = directoryMatchesCache && cachedMetadataMatches

		if e.Type == EntryTypeDirectory {
			var previousSubdirObjectID content.ObjectID
			if cachedEntry != nil {
				previousSubdirObjectID = cachedEntry.ObjectID
			}

			e.ObjectID, err = u.UploadDir(fullPath, previousSubdirObjectID)
			if err != nil {
				return content.NullObjectID, err
			}

			if cachedEntry != nil && e.ObjectID != cachedEntry.ObjectID {
				directoryMatchesCache = false
			}
		} else if cachedMetadataMatches {
			// Avoid hashing by reusing previous object ID.
			e.ObjectID = cachedEntry.ObjectID
		} else {
			e.ObjectID, err = u.UploadFile(fullPath)
			if err != nil {
				return content.NullObjectID, fmt.Errorf("unable to hash file: %s", err)
			}
		}
	}

	if directoryMatchesCache && previous != "" {
		return previous, nil
	}

	writer := u.mgr.NewWriter(
		cas.WithDescription("DIR:"+path),
		cas.WithBlockNamePrefix("D"),
	)
	defer writer.Close()

	WriteDir(writer, listing)

	return writer.Result(true)
}

func (u *uploader) Cancel() {
	atomic.StoreInt32(&u.cancelled, 1)
}

// NewUploader creates new Uploader object for the specified ObjectManager
func NewUploader(mgr cas.ObjectManager) (Uploader, error) {
	u := &uploader{
		mgr:    mgr,
		lister: &filesystemLister{},
	}

	return u, nil
}