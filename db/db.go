package db

import (
	"errors"
	"time"

	"github.com/concourse/atc"
)

type DB interface {
	GetBuild(buildID int) (Build, error)
	GetAllBuilds() ([]Build, error)
	GetAllStartedBuilds() ([]Build, error)

	GetJobBuild(job string, build string) (Build, error)
	GetAllJobBuilds(job string) ([]Build, error)
	GetCurrentBuild(job string) (Build, error)
	GetJobFinishedAndNextBuild(job string) (*Build, *Build, error)

	GetBuildResources(buildID int) ([]BuildInput, []BuildOutput, error)

	CreateJobBuild(job string) (Build, error)

	GetJobBuildForInputs(job string, inputs []BuildInput) (Build, error)
	CreateJobBuildWithInputs(job string, inputs []BuildInput) (Build, error)

	CreateOneOffBuild() (Build, error)

	ScheduleBuild(buildID int, serial bool) (bool, error)
	StartBuild(buildID int, engineName, engineMetadata string) (bool, error)
	FinishBuild(buildID int, status Status) error

	GetBuildEvents(buildID int, from uint) (EventSource, error)
	SaveBuildEvent(buildID int, event atc.Event) error

	SaveBuildInput(buildID int, input BuildInput) (SavedVersionedResource, error)
	SaveBuildOutput(buildID int, vr VersionedResource) (SavedVersionedResource, error)

	SaveResourceVersions(atc.ResourceConfig, []atc.Version) error
	GetLatestVersionedResource(resource string) (SavedVersionedResource, error)
	EnableVersionedResource(resourceID int) error
	DisableVersionedResource(resourceID int) error

	GetLatestInputVersions([]atc.JobBuildInput) ([]BuildInput, error)

	GetNextPendingBuild(job string) (Build, []BuildInput, error)

	GetResourceHistory(resource string) ([]*VersionHistory, error)

	AcquireWriteLockImmediately(locks []NamedLock) (Lock, error)
	AcquireWriteLock(locks []NamedLock) (Lock, error)
	AcquireReadLock(locks []NamedLock) (Lock, error)
	ListLocks() ([]string, error)

	SaveBuildEngineMetadata(buildID int, engineMetadata string) error

	AbortBuild(buildID int) error
	AbortNotifier(buildID int) (Notifier, error)

	Workers() ([]WorkerInfo, error) // auto-expires workers based on ttl
	SaveWorker(WorkerInfo, time.Duration) error
}

//go:generate counterfeiter . Notifier

type Notifier interface {
	Notify() <-chan struct{}
	Close() error
}

//go:generate counterfeiter . ConfigDB

type ConfigDB interface {
	GetConfig() (atc.Config, ConfigID, error)
	SaveConfig(atc.Config, ConfigID) error
}

// sequence identifier used for compare-and-swap
type ConfigID int

var ErrConfigComparisonFailed = errors.New("comparison with existing config failed during save")

//go:generate counterfeiter . Lock

type Lock interface {
	Release() error
}

var ErrEndOfBuildEventStream = errors.New("end of build event stream")
var ErrBuildEventStreamClosed = errors.New("build event stream closed")

//go:generate counterfeiter . EventSource

type EventSource interface {
	Next() (atc.Event, error)
	Close() error
}

type BuildInput struct {
	Name string

	VersionedResource

	FirstOccurrence bool
}

type BuildOutput struct {
	VersionedResource
}

type VersionHistory struct {
	VersionedResource SavedVersionedResource
	InputsTo          []*JobHistory
	OutputsOf         []*JobHistory
}

type JobHistory struct {
	JobName string
	Builds  []Build
}

type WorkerInfo struct {
	Addr string

	ActiveContainers int
	ResourceTypes    []atc.WorkerResourceType
	Platform         string
	Tags             []string
}
