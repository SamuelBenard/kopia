// Package ignorefs implements a wrapper that hides ignored files listed in '.kopiaignore' and in policies attached to directories.
package ignorefs

import (
	"bufio"
	"context"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/ignore"
)

// IgnoreCallback is a function called by ignorefs to report whenever a file or directory is being ignored while listing its parent.
type IgnoreCallback func(path string, metadata fs.Entry)

// FilesPolicyGetter fetches FilesPolicy for a path relative to the root of the filesystem.
// relativePath always starts with "." and path elements are separated with "/".
type FilesPolicyGetter interface {
	GetPolicyForPath(relativePath string) (*FilesPolicy, error)
}

// FilesPolicyMap implements FilesPolicyGetter for a static mapping of relative paths to FilesPolicy.
type FilesPolicyMap map[string]*FilesPolicy

// GetPolicyForPath returns FilePolicy defined in the map or nil.
func (m FilesPolicyMap) GetPolicyForPath(relativePath string) (*FilesPolicy, error) {
	return m[relativePath], nil
}

type ignoreContext struct {
	parent *ignoreContext

	policyGetter FilesPolicyGetter
	onIgnore     []IgnoreCallback

	dotIgnoreFiles []string         // which files to look for more ignore rules
	matchers       []ignore.Matcher // current set of rules to ignore files
	maxFileSize    int64            // maximum size of file allowed
}

func (c *ignoreContext) shouldIncludeByName(path string, e fs.Entry) bool {
	for _, m := range c.matchers {
		if m(path, e.IsDir()) {
			for _, oi := range c.onIgnore {
				oi(path, e)
			}

			return false
		}
	}

	if c.parent == nil {
		return true
	}

	return c.parent.shouldIncludeByName(path, e)
}

type ignoreDirectory struct {
	relativePath  string
	parentContext *ignoreContext

	fs.Directory
}

func (d *ignoreDirectory) Readdir(ctx context.Context) (fs.Entries, error) {
	entries, err := d.Directory.Readdir(ctx)
	if err != nil {
		return nil, err
	}

	thisContext, err := d.buildContext(ctx, entries)
	if err != nil {
		return nil, err
	}

	result := make(fs.Entries, 0, len(entries))

	for _, e := range entries {
		if !thisContext.shouldIncludeByName(d.relativePath+"/"+e.Name(), e) {
			continue
		}

		if maxSize := thisContext.maxFileSize; maxSize > 0 && e.Size() > maxSize {
			continue
		}

		if dir, ok := e.(fs.Directory); ok {
			e = &ignoreDirectory{d.relativePath + "/" + e.Name(), thisContext, dir}
		}

		result = append(result, e)
	}

	return result, nil
}

func (d *ignoreDirectory) buildContext(ctx context.Context, entries fs.Entries) (*ignoreContext, error) {
	var effectiveDotIgnoreFiles = d.parentContext.dotIgnoreFiles

	policy, err := d.parentContext.policyGetter.GetPolicyForPath(d.relativePath)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get policy for %q", d.relativePath)
	}

	if policy != nil {
		effectiveDotIgnoreFiles = policy.DotIgnoreFiles
	}

	var foundDotIgnoreFiles bool

	for _, dotfile := range effectiveDotIgnoreFiles {
		if e := entries.FindByName(dotfile); e != nil {
			foundDotIgnoreFiles = true
		}
	}

	if !foundDotIgnoreFiles && policy == nil {
		// no dotfiles and no policy at this level, reuse parent ignore rules
		return d.parentContext, nil
	}

	newic := &ignoreContext{
		parent:         d.parentContext,
		policyGetter:   d.parentContext.policyGetter,
		onIgnore:       d.parentContext.onIgnore,
		dotIgnoreFiles: effectiveDotIgnoreFiles,
		maxFileSize:    d.parentContext.maxFileSize,
	}

	if policy != nil {
		if err := newic.overrideFromPolicy(policy, d.relativePath); err != nil {
			return nil, err
		}
	}

	if err := newic.loadDotIgnoreFiles(ctx, d.relativePath, entries, effectiveDotIgnoreFiles); err != nil {
		return nil, err
	}

	return newic, nil
}

func (c *ignoreContext) overrideFromPolicy(policy *FilesPolicy, dirPath string) error {
	if policy.NoParentDotIgnoreFiles {
		c.dotIgnoreFiles = nil
	}

	if policy.NoParentIgnoreRules {
		c.matchers = nil
	}

	c.dotIgnoreFiles = combineAndDedupe(c.dotIgnoreFiles, policy.DotIgnoreFiles)
	if policy.MaxFileSize != 0 {
		c.maxFileSize = policy.MaxFileSize
	}

	// append policy-level rules
	for _, rule := range policy.IgnoreRules {
		m, err := ignore.ParseGitIgnore(dirPath, rule)
		if err != nil {
			return errors.Wrapf(err, "unable to parse ignore entry %v", dirPath)
		}

		c.matchers = append(c.matchers, m)
	}

	return nil
}

func (c *ignoreContext) loadDotIgnoreFiles(ctx context.Context, dirPath string, entries fs.Entries, dotIgnoreFiles []string) error {
	for _, dotIgnoreFile := range dotIgnoreFiles {
		e := entries.FindByName(dotIgnoreFile)
		if e == nil {
			// no dotfile
			continue
		}

		f, ok := e.(fs.File)
		if !ok {
			// not a file
			continue
		}

		matchers, err := parseIgnoreFile(ctx, dirPath, f)
		if err != nil {
			return errors.Wrapf(err, "unable to parse ignore file %v", f.Name())
		}

		c.matchers = append(c.matchers, matchers...)
	}

	return nil
}

func combineAndDedupe(slices ...[]string) []string {
	var result []string

	existing := map[string]bool{}

	for _, slice := range slices {
		for _, it := range slice {
			if existing[it] {
				continue
			}

			existing[it] = true

			result = append(result, it)
		}
	}

	return result
}

func parseIgnoreFile(ctx context.Context, baseDir string, file fs.File) ([]ignore.Matcher, error) {
	f, err := file.Open(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open ignore file")
	}
	defer f.Close() //nolint:errcheck

	var matchers []ignore.Matcher

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()

		if strings.HasPrefix(line, "#") {
			// ignore comments
			continue
		}

		if strings.TrimSpace(line) == "" {
			// ignore empty lines
			continue
		}

		m, err := ignore.ParseGitIgnore(baseDir, line)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to parse ignore entry %v", line)
		}

		matchers = append(matchers, m)
	}

	return matchers, nil
}

// Option modifies the behavior of ignorefs
type Option func(parentContext *ignoreContext)

// New returns a fs.Directory that wraps another fs.Directory and hides files specified in the ignore dotfiles.
func New(dir fs.Directory, policyGetter FilesPolicyGetter, options ...Option) fs.Directory {
	if policyGetter == nil {
		policyGetter = FilesPolicyMap{}
	}

	rootContext := &ignoreContext{
		policyGetter: policyGetter,
	}

	for _, opt := range options {
		opt(rootContext)
	}

	return &ignoreDirectory{".", rootContext, dir}
}

var _ fs.Directory = &ignoreDirectory{}

// ReportIgnoredFiles returns an Option causing ignorefs to call the provided function whenever a file or directory is ignored.
func ReportIgnoredFiles(f IgnoreCallback) Option {
	return func(ic *ignoreContext) {
		if f != nil {
			ic.onIgnore = append(ic.onIgnore, f)
		}
	}
}
