package tuf

import (
	"net/http"
	"net/url"
	"os"
	"regexp"

	"github.com/pkg/errors"
)

const (
	tufURLScheme = "https"
	tufAPIFormat = `/v2/%s/_trust/tuf/%s.json`
	healthzPath  = `/_notary_server/health`
	roleRegex    = `^root$|^[1-9]*[0-9]+\.root$|^snapshot$|^timestamp$|^targets$`
	// http headers
	cacheControl       = "Cache-Control"
	cachePolicyNoStore = "no-store"

	maxDelegationCount = 50
)

type tester interface {
	test([]byte) error
}

type rootOptions struct {
	version int
}

type roleOptions struct {
	expectedLength int64
	tests          []tester
}

type repoOptions struct {
	rootOptions
	roleOptions
}

func withRootVersion(version int) repoOption {
	return func(opts *repoOptions) {
		opts.rootOptions.version = version
	}
}

func withRoleExpectedLength(len int64) repoOption {
	return func(opts *repoOptions) {
		opts.roleOptions.expectedLength = len
	}
}

func withRoleTest(t tester) repoOption {
	return func(opts *repoOptions) {
		opts.roleOptions.tests = append(opts.roleOptions.tests, t)
	}
}

type repoOption func(*repoOptions)

type repo interface {
	root(opts ...repoOption) (*Root, error)
	snapshot(opts ...repoOption) (*Snapshot, error)
	targets(rdr roleFetcher) (*RootTarget, error)
	timestamp() (*Timestamp, error)
}

type remoteRepo interface {
	repo
	ping() error
}

type persistentRepo interface {
	repo
	baseDir() string
}

type localRepo struct {
	repoPath string
}

func (r localRepo) baseDir() string { return r.repoPath }

type notaryRepo struct {
	url             *url.URL
	gun             string
	maxResponseSize int64
	client          *http.Client
}

func newLocalRepo(repoPath string) (*localRepo, error) {
	// TODO: remove, repo path is already validated in settings.verify
	err := validatePath(repoPath)
	if err != nil {
		return nil, errors.Wrap(err, "new tuf repo")
	}
	repo := localRepo{
		repoPath: repoPath,
	}

	return &repo, nil
}

func newNotaryRepo(settings *Settings, maxResponseSize int64, client *http.Client) (*notaryRepo, error) {
	r := &notaryRepo{
		maxResponseSize: maxResponseSize,
		gun:             settings.GUN,
		client:          client,
	}
	var err error
	// TODO remove, already validated in settings.verify
	r.url, err = validateURL(settings.NotaryURL)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func validateURL(repoURL string) (*url.URL, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return nil, errors.Wrap(err, "tuf remote repo url validation failed")
	}
	if u.Scheme != tufURLScheme {
		return nil, errors.Errorf("tuf url scheme must be %q", tufURLScheme)
	}
	return u, nil
}

// validatePath path must exist and be a directory, or a symlink to a directory
func validatePath(repoPath string) error {
	fi, err := os.Stat(repoPath)
	if os.IsNotExist(err) {
		return errors.Wrap(err, "tuf repo path validation failed")
	}
	if !fi.IsDir() {
		return errors.Errorf("tuf repo path %q must be a directory", repoPath)
	}
	return nil
}

func validateRole(r role) error {
	if !regexp.MustCompile(roleRegex).MatchString(string(r)) {
		return errors.Errorf("%q is not a valid role", r)
	}
	return nil
}

func isRoleCorrect(r role, s interface{}) {
	var hit bool
	switch s.(type) {
	case Root, *Root:
		hit = r == roleRoot
	case Targets, *Targets:
		hit = r == roleTargets
	case Timestamp, *Timestamp:
		hit = r == roleTimestamp
	case Snapshot, *Snapshot:
		hit = r == roleSnapshot
	}
	if !hit {
		panic("Programmer error! Role name and role type mismatch.")
	}
}

// roleFetcher is an abstraction of a thing that fetches Targets.
type roleFetcher interface {
	fetch(path string) (*Targets, error)
}

// 4.5. **Perform a preorder depth-first search for metadata about the desired
// target, beginning with the top-level targets role.**
//
// targetTreeBuilder performs some special root node initialization and then
// recursively calls getDelegatedTarget to do a preorder traversal of the
// Targets tree.
//
// Each time a target node is encountered, it persists path information in proper
// precedence according to the following section.
// 4.5.1. If this role has been visited before, then skip this role (so that
// cycles in the delegation graph are avoided).
// Otherwise, if an application-specific maximum number of roles have been
// visited, then go to step 5 (so that attackers cannot cause the client to
// waste excessive bandwidth or time).
// Otherwise, if this role contains metadata about the desired target, then go
// to step 5.
func targetTreeBuilder(fetcher roleFetcher) (*RootTarget, error) {
	targ, err := fetcher.fetch(string(roleTargets))
	if err != nil {
		return nil, err
	}
	root := RootTarget{
		Targets:      targ,
		paths:        make(FimMap),
		targetLookup: make(map[string]*Targets),
	}
	root.append(string(roleTargets), targ)

	for _, delegation := range root.Signed.Delegations.Roles {
		err = getDelegatedTarget(fetcher, &root, delegation.Name)
		if err != nil {
			return nil, err
		}
	}
	return &root, nil
}

func getDelegatedTarget(fetcher roleFetcher, root *RootTarget, roleName string) error {
	target, err := fetcher.fetch(roleName)
	if err != nil {
		return err
	}
	root.append(roleName, target)
	for _, role := range target.Signed.Delegations.Roles {
		err = getDelegatedTarget(fetcher, root, role.Name)
		// prevent cycles
		if err == errTargetSeen {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
