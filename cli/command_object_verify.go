package cli

import (
	"fmt"
	"log"
	"os"

	"github.com/kopia/kopia/object"
	"github.com/kopia/kopia/snapshot"

	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var (
	verifyCommand               = objectCommands.Command("verify", "Verify the contents of stored object")
	verifyCommandRecursive      = verifyCommand.Flag("recursive", "Recursive verification of directories").Short('r').Bool()
	verifyCommandErrorThreshold = verifyCommand.Flag("max-errors", "Maximum number of errors before stopping").Default("0").Int()
	verifyCommandPath           = verifyCommand.Arg("path", "Path").Required().String()
)

type verifier struct {
	mgr     *snapshot.Manager
	om      *object.ObjectManager
	visited map[string]bool
	errors  []error
}

func (v *verifier) reportError(path string, err error) bool {
	err = fmt.Errorf("error validating %q: %v", path, err)
	log.Printf("%v", err)
	v.errors = append(v.errors, err)
	return len(v.errors) >= *verifyCommandErrorThreshold
}

func (v *verifier) verifyDirectory(oid object.ObjectID, path string) error {
	if v.visited[oid.String()] {
		return nil
	}
	v.visited[oid.String()] = true

	log.Printf("verifying directory %q (%v)", path, oid)

	d := v.mgr.DirectoryEntry(oid)
	entries, err := d.Readdir()
	if err != nil {
		if v.reportError(path, fmt.Errorf("error reading directory %q %v: %v", path, oid, err)) {
			return err
		}
	}

	for _, e := range entries {
		m := e.Metadata()
		objectID := e.(object.HasObjectID).ObjectID()
		childPath := path + "/" + m.Name
		if m.FileMode().IsDir() {
			if *verifyCommandRecursive {
				if err := v.verifyDirectory(objectID, childPath); err != nil {
					if v.reportError(childPath, err) {
						return err
					}
				}
			}
		}

		if err := v.verifyObject(objectID, childPath, m.FileSize); err != nil {
			if v.reportError(childPath, err) {
				return err
			}
		}
	}

	return nil
}

func (v *verifier) verifyObject(oid object.ObjectID, path string, expectedLength int64) error {
	if v.visited[oid.String()] {
		return nil
	}
	v.visited[oid.String()] = true

	if expectedLength < 0 {
		log.Printf("verifying object %v", oid)
	} else {
		log.Printf("verifying object %v (%v) with length %v", path, oid, expectedLength)
	}

	length, _, err := v.om.VerifyObject(oid)
	if err != nil {
		return fmt.Errorf("invalid object %q: %v", oid, err)
	}

	if expectedLength == -1 {
		log.Printf("object length: %v", length)
	} else if length != expectedLength {
		return fmt.Errorf("invalid object length %q, %v, expected %v", oid, length, expectedLength)
	}

	return nil
}

func runVerifyCommand(context *kingpin.ParseContext) error {
	rep := mustOpenRepository(nil)
	defer rep.Close()

	mgr := snapshot.NewManager(rep)

	oid, err := parseObjectID(mgr, *verifyCommandPath)
	if err != nil {
		return err
	}

	v := verifier{
		mgr,
		rep.Objects,
		make(map[string]bool),
		nil,
	}

	if *verifyCommandRecursive {
		v.verifyDirectory(oid, oid.String())
	}

	v.verifyObject(oid, oid.String(), -1)

	if len(v.errors) == 0 {
		return nil
	}

	if len(v.errors) == 1 {
		return v.errors[0]
	}

	for i, e := range v.errors {
		fmt.Fprintf(os.Stderr, "  %-3v: %v\n", i, e)
	}

	return fmt.Errorf("encountered %v errors", len(v.errors))
}

func init() {
	verifyCommand.Action(runVerifyCommand)
}