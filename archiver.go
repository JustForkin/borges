package borges

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/src-d/go-billy.v2"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/storer"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/client"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/server"
	"gopkg.in/src-d/go-git.v4/storage/filesystem"
	"gopkg.in/src-d/go-kallax.v1"
	"srcd.works/core-retrieval.v0/repository"
	"srcd.works/core.v0/model"
	"srcd.works/go-errors.v0"
)

var (
	ErrCleanRepositoryDir     = errors.NewKind("cleaning up local repo dir failed")
	ErrFetch                  = errors.NewKind("fetching %s failed")
	ErrPushToRootedRepository = errors.NewKind("push to rooted repo %s failed")
	ErrArchivingRoots         = errors.NewKind("archiving %d out of %d roots failed: %s")
	ErrEndpointsEmpty         = errors.NewKind("endpoints is empty")
	ErrRepositoryIDNotFound   = errors.NewKind("repository id not found: %s")
)

// Archiver archives repositories. Archiver instances are thread-safe and can
// be reused.
//
// See borges documentation for more details about the archiving rules.
type Archiver struct {
	Notifiers struct {
		// Start function, if set, is called whenever a job is started.
		Start func(*Job)
		// Stop function, if set, is called whenever a job stops. If
		// there was an error, it is passed as second parameter,
		// otherwise, it is nil.
		Stop func(*Job, error)
		// Warn function, if set, is called whenever there is a warning
		// during the processing of a repository.
		Warn func(*Job, error)
	}

	// Temp is the filesystem used to fetch repositories to.
	Temp billy.Filesystem

	// RepositoryStore is the database where repository models are stored.
	RepositoryStorage *model.RepositoryStore

	// RootedTransactioner is used to push new references to our repository
	// storage.
	RootedTransactioner repository.RootedTransactioner
}

func NewArchiver(r *model.RepositoryStore, tx repository.RootedTransactioner,
	tmpFs billy.Filesystem) *Archiver {
	return &Archiver{
		Temp:                tmpFs,
		RepositoryStorage:   r,
		RootedTransactioner: tx,
	}
}

// Do archives a repository according to a job.
func (a *Archiver) Do(j *Job) error {
	a.notifyStart(j)
	err := a.do(j)
	a.notifyStop(j, err)
	return err
}

func (a *Archiver) do(j *Job) error {
	r, err := a.getRepositoryModel(j)
	if err != nil {
		return err
	}

	endpoint, err := selectEndpoint(r.Endpoints)
	if err != nil {
		return err
	}

	dir := a.newTempRepoDir(j)
	defer billy.RemoveAll(a.Temp, dir)
	tmpFs := a.Temp.Dir(dir)

	gr, err := createLocalRepository(tmpFs, endpoint, r.References)
	if err != nil {
		return err
	}

	if err := fetchAll(gr); err != nil {
		if err == git.NoErrAlreadyUpToDate ||
			err == transport.ErrEmptyUploadPackRequest {
			//TODO: Update repository timestamp
			return nil
		}

		return ErrFetch.Wrap(err, endpoint)
	}

	changes, err := NewChanges(r.References, gr)
	if err != nil {
		return err
	}

	if err := a.pushChangesToRootedRepositories(j, r, gr, changes); err != nil {
		return err
	}

	//TODO: Update repository
	return nil
}

func (a *Archiver) getRepositoryModel(j *Job) (*model.Repository, error) {
	q := model.NewRepositoryQuery().FindByID(kallax.ULID(j.RepositoryID))
	r, err := a.RepositoryStorage.FindOne(q)
	if err != nil {
		return nil, ErrRepositoryIDNotFound.Wrap(err, j.RepositoryID.String())
	}

	return r, nil
}

func (a *Archiver) newTempRepoDir(j *Job) string {
	return filepath.Join("local_repos",
		j.RepositoryID.String(),
		strconv.FormatInt(time.Now().UnixNano(), 10),
	)
}

func (a *Archiver) cleanRepoDir(j *Job, dir string) {
	if err := billy.RemoveAll(a.Temp, dir); err != nil {
		a.Notifiers.Warn(j, ErrCleanRepositoryDir.Wrap(err))
	}
}

func (a *Archiver) notifyStart(j *Job) {
	if a.Notifiers.Start == nil {
		return
	}

	a.Notifiers.Start(j)
}

func (a *Archiver) notifyStop(j *Job, err error) {
	if a.Notifiers.Stop == nil {
		return
	}

	a.Notifiers.Stop(j, err)
}

func (a *Archiver) notifyWarn(j *Job, err error) {
	if a.Notifiers.Warn == nil {
		return
	}

	a.Notifiers.Warn(j, err)
}

func selectEndpoint(endpoints []string) (string, error) {
	if len(endpoints) == 0 {
		return "", ErrEndpointsEmpty.New()
	}

	// TODO check which endpoint to use
	return endpoints[0], nil
}

// createLocalRepository creates a new repository with some predefined references
// hardcoded into his storage. This is intended to be able to do a partial fetch.
// Having the references into the storage we will only download new objects, not
// the entire repository.
func createLocalRepository(dir billy.Filesystem, endpoint string, refs []*model.Reference) (*git.Repository, error) {
	s, err := filesystem.NewStorage(dir)
	if err != nil {
		return nil, err
	}

	r, err := git.Init(s, nil)
	if err != nil {
		return nil, err
	}

	if err := setReferences(s, refs...); err != nil {
		return nil, err
	}

	c := &config.RemoteConfig{
		Name: "origin",
		URL:  endpoint,
	}
	if _, err := r.CreateRemote(c); err != nil {
		return nil, err
	}

	return r, nil
}

func setReferences(s storer.ReferenceStorer, refs ...*model.Reference) error {
	for _, ref := range refs {
		if err := s.SetReference(ref.GitReference()); err != nil {
			return err
		}
	}

	return nil
}

func fetchAll(r *git.Repository) error {
	o := &git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("+refs/heads/*:refs/heads/*")},
	}

	return r.Fetch(o)
}

func (a *Archiver) pushChangesToRootedRepositories(j *Job, r *model.Repository,
	lr *git.Repository, changes Changes) error {
	var failedInits []model.SHA1
	for ic, cs := range changes {
		//TODO: try lock first_commit
		//TODO: if lock cannot be acquired after timeout, continue
		if err := a.pushChangesToRootedRepository(r, lr, ic, cs); err != nil {
			err = ErrPushToRootedRepository.Wrap(err, ic.String())
			a.notifyWarn(j, err)
			failedInits = append(failedInits, ic)
			//TODO: release lock
			continue
		}
		//TODO: update references db with the changes
		//TODO: release lock
	}

	return checkFailedInits(changes, failedInits)
}

func (a *Archiver) pushChangesToRootedRepository(r *model.Repository, lr *git.Repository, ic model.SHA1, changes []*Command) error {
	tx, err := a.RootedTransactioner.Begin(plumbing.Hash(ic))
	if err != nil {
		return err
	}

	rr, err := git.Open(tx.Storer(), nil)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	return withInProcRepository(rr, func(url string) error {
		rm, err := lr.CreateRemote(&config.RemoteConfig{
			Name: ic.String(),
			URL:  url,
		})
		if err != nil {
			_ = tx.Rollback()
			return err
		}

		pushOpts := &git.PushOptions{
			RefSpecs: a.changesToPushRefSpec(r.ID, changes),
		}
		if err := rm.Push(pushOpts); err != nil {
			_ = tx.Rollback()
			return err
		}

		return tx.Commit()
	})
}

func withInProcRepository(r *git.Repository, f func(string) error) error {
	proto := fmt.Sprintf("borges%d", rand.Uint32())
	url := fmt.Sprintf("%s://%s", proto, "repo")
	ep, err := transport.NewEndpoint(url)
	if err != nil {
		return err
	}

	s := server.NewServer(server.MapLoader{ep: r.Storer})
	client.InstallProtocol(proto, s)
	defer client.InstallProtocol(proto, nil)

	return f(url)
}

func (a *Archiver) changesToPushRefSpec(id kallax.ULID, changes []*Command) []config.RefSpec {
	var rss []config.RefSpec
	for _, ch := range changes {
		var rs string
		switch ch.Action() {
		case Create:
			rs = fmt.Sprintf("+%s:%s/%d", ch.New.Name, ch.New.Name, id)
		case Delete:
			rs = fmt.Sprintf(":%s/%d", ch.Old.Name, id)
		case Update:
			rs = fmt.Sprintf("+%s:%s/%d", ch.New.Name, ch.New.Name, id)
		default:
			panic("not reachable")
		}

		rss = append(rss, config.RefSpec(rs))
	}

	return rss
}

func checkFailedInits(changes Changes, failed []model.SHA1) error {
	n := len(failed)
	if n == 0 {
		return nil
	}

	strs := make([]string, n)
	for i := 0; i < n; i++ {
		strs[i] = failed[i].String()
	}

	return ErrArchivingRoots.New(
		n,
		len(changes),
		strings.Join(strs, ", "),
	)
}

// NewArchiverWorkerPool creates a new WorkerPool that uses an Archiver to
// process jobs. It takes optional start, stop and warn notifier functions that
// are equal to the Archiver notifiers but with additional WorkerContext.
func NewArchiverWorkerPool(r *model.RepositoryStore,
	tx repository.RootedTransactioner,
	tmpFs billy.Filesystem,
	start func(*WorkerContext, *Job),
	stop func(*WorkerContext, *Job, error),
	warn func(*WorkerContext, *Job, error)) *WorkerPool {

	do := func(ctx *WorkerContext, j *Job) error {
		a := NewArchiver(r, tx, tmpFs)

		if start != nil {
			a.Notifiers.Start = func(j *Job) {
				start(ctx, j)
			}
		}

		if stop != nil {
			a.Notifiers.Stop = func(j *Job, err error) {
				stop(ctx, j, err)
			}
		}

		if warn != nil {
			a.Notifiers.Warn = func(j *Job, err error) {
				warn(ctx, j, err)
			}
		}

		return a.Do(j)
	}

	return NewWorkerPool(do)
}
