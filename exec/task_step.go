package exec

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/garden"
	"github.com/concourse/atc"
	"github.com/concourse/atc/db"
	"github.com/concourse/atc/resource"
	"github.com/concourse/atc/worker"
	"github.com/concourse/baggageclaim"
	"github.com/pivotal-golang/lager"
)

const taskProcessPropertyName = "concourse:task-process"
const taskExitStatusPropertyName = "concourse:exit-status"

// MissingInputsError is returned when any of the task's required inputs are
// missing.
type MissingInputsError struct {
	Inputs []string
}

// Error prints a human-friendly message listing the inputs that were missing.
func (err MissingInputsError) Error() string {
	return fmt.Sprintf("missing inputs: %s", strings.Join(err.Inputs, ", "))
}

// ErrImageUnavailable is returned when a task's configured image resource
// has no versions.
var ErrImageUnavailable = errors.New("no versions of image available")

type ErrImageGetDidNotProduceVolume error

func NewErrImageGetDidNotProduceVolume(taskName string) ErrImageGetDidNotProduceVolume {
	return fmt.Errorf("getting the image for task '%s' did not produce a volume", taskName)
}

// TaskStep executes a TaskConfig, whose inputs will be fetched from the
// SourceRepository and outputs will be added to the SourceRepository.
type TaskStep struct {
	logger         lager.Logger
	sourceName     SourceName
	containerID    worker.Identifier
	metadata       worker.Metadata
	tags           atc.Tags
	delegate       TaskDelegate
	privileged     Privileged
	configSource   TaskConfigSource
	workerPool     worker.Client
	artifactsRoot  string
	trackerFactory TrackerFactory

	repo *SourceRepository

	container worker.Container
	process   garden.Process

	exitStatus int
}

func newTaskStep(
	logger lager.Logger,
	sourceName SourceName,
	containerID worker.Identifier,
	metadata worker.Metadata,
	tags atc.Tags,
	delegate TaskDelegate,
	privileged Privileged,
	configSource TaskConfigSource,
	workerPool worker.Client,
	artifactsRoot string,
	trackerFactory TrackerFactory,
) TaskStep {
	return TaskStep{
		logger:         logger,
		sourceName:     sourceName,
		containerID:    containerID,
		metadata:       metadata,
		tags:           tags,
		delegate:       delegate,
		privileged:     privileged,
		configSource:   configSource,
		workerPool:     workerPool,
		artifactsRoot:  artifactsRoot,
		trackerFactory: trackerFactory,
	}
}

// Using finishes construction of the TaskStep and returns a *TaskStep. If the
// *TaskStep errors, its error is reported to the delegate.
func (step TaskStep) Using(prev Step, repo *SourceRepository) Step {
	step.repo = repo

	return errorReporter{
		Step:          &step,
		ReportFailure: step.delegate.Failed,
	}
}

// Run will first load the TaskConfig. A worker will be selected based on the
// TaskConfig's platform, the TaskStep's tags, and prioritized by availability
// of volumes for the TaskConfig's inputs. Inputs that did not have volumes
// available on the worker will be streamed in to the container.
//
// If any inputs are not available in the SourceRepository, MissingInputsError
// is returned.
//
// Once all the inputs are satisfies, the task's script will be executed, and
// the RunStep indicates that it's ready, and any signals will be forwarded to
// the script.
//
// If the script exits successfully, the outputs specified in the TaskConfig
// are registered with the SourceRepository. If no outputs are specified, the
// task's entire working directory is registered as an ArtifactSource under the
// name of the task.
func (step *TaskStep) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	var err error
	var found bool

	processIO := garden.ProcessIO{
		Stdout: step.delegate.Stdout(),
		Stderr: step.delegate.Stderr(),
	}

	config, err := step.configSource.FetchConfig(step.repo)
	if err != nil {
		return err
	}

	step.metadata.EnvironmentVariables = step.envForParams(config.Params)

	runContainerID := step.containerID
	runContainerID.Stage = db.ContainerStageRun

	step.container, found, err = step.workerPool.FindContainerForIdentifier(
		step.logger.Session("found-container"),
		runContainerID,
	)

	if err == nil && found {
		exitStatusProp, err := step.container.Property(taskExitStatusPropertyName)
		if err == nil {
			step.logger.Info("already-exited", lager.Data{"status": exitStatusProp})

			// process already completed; recover result

			_, err = fmt.Sscanf(exitStatusProp, "%d", &step.exitStatus)
			if err != nil {
				return err
			}

			step.registerSource(config)
			return nil
		}

		processID, err := step.container.Property(taskProcessPropertyName)
		if err != nil {
			// rogue container? perhaps did not shut down cleanly.
			return err
		}

		step.logger.Info("already-running", lager.Data{"process-id": processID})

		// process still running; re-attach
		step.process, err = step.container.Attach(processID, processIO)
		if err != nil {
			return err
		}

		step.logger.Info("attached")
	} else {
		// container does not exist; new session

		step.delegate.Initializing(config)

		workerSpec := worker.WorkerSpec{
			Platform: config.Platform,
			Tags:     step.tags,
		}

		if config.ImageResource != nil {
			workerSpec.ResourceType = config.ImageResource.Type
		}

		var inputsToStream []inputPair
		compatibleWorkers, err := step.workerPool.AllSatisfying(workerSpec)
		if err != nil {
			return err
		}

		step.container, inputsToStream, err = step.createContainer(compatibleWorkers, config, signals)
		if err != nil {
			return err
		}

		err = step.ensureBuildDirExists(step.container)
		if err != nil {
			return err
		}

		err = step.streamInputs(inputsToStream)
		if err != nil {
			return err
		}

		err = step.setupOutputs(config.Outputs)
		if err != nil {
			return err
		}

		step.delegate.Started()

		step.process, err = step.container.Run(garden.ProcessSpec{
			Path: config.Run.Path,
			Args: config.Run.Args,
			Env:  step.envForParams(config.Params),

			Dir:  step.artifactsRoot,
			User: "root",
			TTY:  &garden.TTYSpec{},
		}, processIO)
		if err != nil {
			return err
		}

		err = step.container.SetProperty(taskProcessPropertyName, step.process.ID())
		if err != nil {
			return err
		}
	}

	close(ready)

	waitExitStatus := make(chan int, 1)
	waitErr := make(chan error, 1)
	go func() {
		status, err := step.process.Wait()
		if err != nil {
			waitErr <- err
		} else {
			waitExitStatus <- status
		}
	}()

	select {
	case <-signals:
		step.registerSource(config)

		step.container.Stop(false)
		return ErrInterrupted

	case status := <-waitExitStatus:
		step.registerSource(config)

		step.exitStatus = status

		statusValue := fmt.Sprintf("%d", status)

		err := step.container.SetProperty(taskExitStatusPropertyName, statusValue)
		if err != nil {
			return err
		}

		step.delegate.Finished(ExitStatus(status))

		return nil

	case err := <-waitErr:
		return err
	}
}

func (step *TaskStep) createContainer(compatibleWorkers []worker.Worker, config atc.TaskConfig, signals <-chan os.Signal) (worker.Container, []inputPair, error) {
	chosenWorker, inputMounts, inputsToStream, err := step.chooseWorkerWithMostVolumes(compatibleWorkers, config.Inputs)
	if err != nil {
		return nil, nil, err
	}

	outputMounts := []worker.VolumeMount{}
	for _, output := range config.Outputs {
		path := artifactsPath(output, step.artifactsRoot)

		baggageclaimClient, found := chosenWorker.VolumeManager()
		if !found {
			break
		}

		ourVolume, volErr := baggageclaimClient.CreateVolume(step.logger, baggageclaim.VolumeSpec{
			Properties: baggageclaim.VolumeProperties{},
			TTL:        5 * time.Minute,
			Privileged: bool(step.privileged),
		})
		if volErr != nil {
			return nil, nil, volErr
		}

		outputMounts = append(outputMounts, worker.VolumeMount{
			Volume:    ourVolume,
			MountPath: path,
		})
	}

	containerSpec := worker.TaskContainerSpec{
		Platform:   config.Platform,
		Tags:       step.tags,
		Privileged: bool(step.privileged),
		Inputs:     inputMounts,
		Outputs:    outputMounts,
	}

	var imageResource resource.Resource

	if config.ImageResource != nil {
		imageResource, err = step.getContainerImage(signals, chosenWorker, config)
		if err != nil {
			return nil, nil, err
		}

		imageVolume, found, err := imageResource.CacheVolume()
		if err != nil {
			return nil, nil, err
		}

		if !found {
			return nil, nil, NewErrImageGetDidNotProduceVolume(string(step.sourceName))
		}

		containerSpec.ImageVolume = imageVolume
	} else {
		containerSpec.Image = config.Image
	}

	runContainerID := step.containerID
	runContainerID.Stage = db.ContainerStageRun
	container, err := chosenWorker.CreateContainer(
		step.logger.Session("created-container"),
		runContainerID,
		step.metadata,
		containerSpec,
	)

	if err != nil {
		step.logger.Info(err.Error())
		newCompatibleWorkers := make([]worker.Worker, 0, len(compatibleWorkers)-1)
		for _, worker := range compatibleWorkers {
			if worker != chosenWorker {
				newCompatibleWorkers = append(newCompatibleWorkers, worker)
			}
		}

		if len(newCompatibleWorkers) == 0 {
			return nil, nil, errors.New("Failed to create container on all compatible workers")
		}
		return step.createContainer(newCompatibleWorkers, config, signals)
	}

	if imageResource != nil {
		imageResource.Release(0)
	}

	for _, mount := range inputMounts {
		// stop heartbeating ourselves now that container has picked up the
		// volumes
		mount.Volume.Release(0)
	}

	for _, mount := range outputMounts {
		// stop heartbeating ourselves now that container has picked up the
		// volumes
		mount.Volume.Release(0)
	}

	return container, inputsToStream, nil
}

func (step *TaskStep) registerSource(config atc.TaskConfig) {
	if len(config.Outputs) == 0 {
		step.repo.RegisterSource(step.sourceName, step)
	} else {
		volumeMounts := step.container.VolumeMounts()

		for _, output := range config.Outputs {
			if len(volumeMounts) > 0 {
				outputPath := artifactsPath(output, step.artifactsRoot)

				for _, mount := range volumeMounts {
					if mount.MountPath == outputPath {
						source := newContainerSource(step.artifactsRoot, step.container, output, step.logger, mount.Volume.Handle())
						step.repo.RegisterSource(SourceName(output.Name), source)
					}
				}
			} else {
				source := newContainerSource(step.artifactsRoot, step.container, output, step.logger, "")
				step.repo.RegisterSource(SourceName(output.Name), source)
			}
		}
	}
}

// Result indicates Success as true if the script's exit status was 0.
//
// It also indicates ExitStatus as the exit status of the script.
//
// All other types are ignored.
func (step *TaskStep) Result(x interface{}) bool {
	switch v := x.(type) {
	case *Success:
		*v = step.exitStatus == 0
		return true

	case *ExitStatus:
		*v = ExitStatus(step.exitStatus)
		return true

	default:
		return false
	}
}

// Release releases the created container for either SuccessfulStepTTL or
// FailedStepTTL.
func (step *TaskStep) Release() {
	if step.container == nil {
		return
	}

	if step.exitStatus == 0 {
		step.container.Release(SuccessfulStepTTL)
	} else {
		step.container.Release(FailedStepTTL)
	}
}

// StreamFile streams the given file out of the task's container.
func (step *TaskStep) StreamFile(source string) (io.ReadCloser, error) {
	out, err := step.container.StreamOut(garden.StreamOutSpec{
		Path: path.Join(step.artifactsRoot, source),
	})

	if err != nil {
		return nil, err
	}

	tarReader := tar.NewReader(out)

	_, err = tarReader.Next()
	if err != nil {
		return nil, FileNotFoundError{Path: source}
	}

	return fileReadCloser{
		Reader: tarReader,
		Closer: out,
	}, nil
}

// StreamTo streams the task's entire working directory to the destination.
func (step *TaskStep) StreamTo(destination ArtifactDestination) error {
	out, err := step.container.StreamOut(garden.StreamOutSpec{
		Path: step.artifactsRoot + "/",
	})
	if err != nil {
		return err
	}

	return destination.StreamIn(".", out)
}

// VolumeOn returns nothing.
func (step *TaskStep) VolumeOn(worker worker.Worker) (baggageclaim.Volume, bool, error) {
	return nil, false, nil
}

func (step *TaskStep) chooseWorkerWithMostVolumes(compatibleWorkers []worker.Worker, inputs []atc.TaskInputConfig) (worker.Worker, []worker.VolumeMount, []inputPair, error) {
	inputMounts := []worker.VolumeMount{}
	inputsToStream := []inputPair{}

	var chosenWorker worker.Worker
	for _, w := range compatibleWorkers {
		mounts, toStream, err := step.inputsOn(inputs, w)
		if err != nil {
			return nil, nil, nil, err
		}

		if len(mounts) >= len(inputMounts) {
			for _, mount := range inputMounts {
				mount.Volume.Release(0)
			}

			inputMounts = mounts
			inputsToStream = toStream
			chosenWorker = w
		} else {
			for _, mount := range mounts {
				mount.Volume.Release(0)
			}
		}
	}

	return chosenWorker, inputMounts, inputsToStream, nil
}

type inputPair struct {
	input  atc.TaskInputConfig
	source ArtifactSource
}

func (step *TaskStep) inputsOn(inputs []atc.TaskInputConfig, chosenWorker worker.Worker) ([]worker.VolumeMount, []inputPair, error) {
	var mounts []worker.VolumeMount

	var inputPairs []inputPair

	var missingInputs []string

	for _, input := range inputs {
		source, found := step.repo.SourceFor(SourceName(input.Name))
		if !found {
			missingInputs = append(missingInputs, input.Name)
			continue
		}

		volume, existsOnWorker, err := source.VolumeOn(chosenWorker)
		if err != nil {
			return nil, nil, err
		}

		if existsOnWorker {
			mounts = append(mounts, worker.VolumeMount{
				Volume:    volume,
				MountPath: step.inputDestination(input),
			})
		} else {
			inputPairs = append(inputPairs, inputPair{
				input:  input,
				source: source,
			})
		}
	}

	if len(missingInputs) > 0 {
		return nil, nil, MissingInputsError{missingInputs}
	}

	return mounts, inputPairs, nil
}

func (step *TaskStep) inputDestination(config atc.TaskInputConfig) string {
	subdir := config.Path
	if config.Path == "" {
		subdir = config.Name
	}

	return filepath.Join(step.artifactsRoot, subdir)
}

func (step *TaskStep) getContainerImage(signals <-chan os.Signal, worker worker.Client, config atc.TaskConfig) (resource.Resource, error) {
	tracker := step.trackerFactory.TrackerFor(worker)

	resourceType := resource.ResourceType(config.ImageResource.Type)

	checkSess := resource.Session{
		ID:       step.containerID,
		Metadata: step.metadata,
	}

	checkSess.ID.Stage = db.ContainerStageCheck
	checkSess.ID.CheckType = config.ImageResource.Type
	checkSess.ID.CheckSource = config.ImageResource.Source
	checkSess.Metadata.Type = db.ContainerTypeCheck
	checkSess.Metadata.WorkingDirectory = ""
	checkSess.Metadata.EnvironmentVariables = nil

	checkingResource, err := tracker.Init(
		step.logger.Session("check-image"),
		resource.EmptyMetadata{},
		checkSess,
		resourceType,
		nil,
	)
	if err != nil {
		return nil, err
	}

	defer checkingResource.Release(0)

	versions, err := checkingResource.Check(config.ImageResource.Source, nil)
	if err != nil {
		return nil, err
	}

	if len(versions) == 0 {
		return nil, ErrImageUnavailable
	}

	getSess := resource.Session{
		ID:       step.containerID,
		Metadata: step.metadata,
	}

	getSess.ID.Stage = db.ContainerStageGet
	getSess.Metadata.Type = db.ContainerTypeGet
	getSess.Metadata.WorkingDirectory = ""
	getSess.Metadata.EnvironmentVariables = nil

	getResource, cache, err := tracker.InitWithCache(
		step.logger.Session("init-image"),
		resource.EmptyMetadata{},
		getSess,
		resourceType,
		nil,
		resource.ResourceCacheIdentifier{
			Type:    resourceType,
			Version: versions[0],
			Source:  config.ImageResource.Source,
		},
	)
	if err != nil {
		return nil, err
	}

	isInitialized, err := cache.IsInitialized()
	if err != nil {
		return nil, err
	}

	if !isInitialized {
		versionedSource := getResource.Get(
			resource.IOConfig{
				Stderr: step.delegate.Stderr(),
			},
			config.ImageResource.Source,
			nil,
			versions[0],
		)

		err := versionedSource.Run(signals, make(chan struct{}))
		if err != nil {
			return nil, err
		}

		cache.Initialize()
	}

	return getResource, nil
}

func (step *TaskStep) ensureBuildDirExists(container garden.Container) error {
	return createContainerDir(container, step.artifactsRoot)
}

func (step *TaskStep) streamInputs(inputPairs []inputPair) error {
	for _, pair := range inputPairs {
		destination := newContainerDestination(
			step.artifactsRoot,
			step.container,
			pair.input,
		)

		err := pair.source.StreamTo(destination)
		if err != nil {
			return err
		}
	}

	return nil
}

func (step *TaskStep) setupOutputs(outputs []atc.TaskOutputConfig) error {
	for _, output := range outputs {
		source := newContainerSource(step.artifactsRoot, step.container, output, step.logger, "")

		err := source.initialize()
		if err != nil {
			return err
		}
	}

	return nil
}

func (TaskStep) mergeTags(tagsOne []string, tagsTwo []string) []string {
	var ret []string

	uniq := map[string]struct{}{}

	for _, tag := range tagsOne {
		uniq[tag] = struct{}{}
	}

	for _, tag := range tagsTwo {
		uniq[tag] = struct{}{}
	}

	for tag := range uniq {
		ret = append(ret, tag)
	}

	return ret
}

func (TaskStep) envForParams(params map[string]string) []string {
	env := make([]string, 0, len(params))

	for k, v := range params {
		env = append(env, k+"="+v)
	}

	return env
}

type containerDestination struct {
	container     garden.Container
	inputConfig   atc.TaskInputConfig
	artifactsRoot string
}

func newContainerDestination(artifactsRoot string, container garden.Container, inputConfig atc.TaskInputConfig) *containerDestination {
	return &containerDestination{
		container:     container,
		inputConfig:   inputConfig,
		artifactsRoot: artifactsRoot,
	}
}

func (dest *containerDestination) StreamIn(dst string, src io.Reader) error {
	inputDst := dest.inputConfig.Path
	if len(inputDst) == 0 {
		inputDst = dest.inputConfig.Name
	}

	return dest.container.StreamIn(garden.StreamInSpec{
		Path:      dest.artifactsRoot + "/" + inputDst + "/" + dst,
		TarStream: src,
	})
}

type containerSource struct {
	container     garden.Container
	outputConfig  atc.TaskOutputConfig
	artifactsRoot string
	volumeHandle  string
	logger        lager.Logger
}

func newContainerSource(
	artifactsRoot string,
	container garden.Container,
	outputConfig atc.TaskOutputConfig,
	logger lager.Logger,
	volumeHandle string,
) *containerSource {
	return &containerSource{
		container:     container,
		outputConfig:  outputConfig,
		artifactsRoot: artifactsRoot,
		volumeHandle:  volumeHandle,
		logger:        logger,
	}
}

func (src *containerSource) StreamTo(destination ArtifactDestination) error {
	out, err := src.container.StreamOut(garden.StreamOutSpec{
		Path: artifactsPath(src.outputConfig, src.artifactsRoot),
	})
	if err != nil {
		return err
	}

	return destination.StreamIn(".", out)
}

func (src *containerSource) StreamFile(filename string) (io.ReadCloser, error) {
	out, err := src.container.StreamOut(garden.StreamOutSpec{
		Path: path.Join(artifactsPath(src.outputConfig, src.artifactsRoot), filename),
	})

	if err != nil {
		return nil, err
	}

	tarReader := tar.NewReader(out)

	_, err = tarReader.Next()
	if err != nil {
		return nil, FileNotFoundError{Path: filename}
	}

	return fileReadCloser{
		Reader: tarReader,
		Closer: out,
	}, nil
}

func (src *containerSource) VolumeOn(w worker.Worker) (baggageclaim.Volume, bool, error) {
	if baggageclaimClient, found := w.VolumeManager(); len(src.volumeHandle) > 0 && found {
		volume, found, err := baggageclaimClient.LookupVolume(src.logger, src.volumeHandle)
		if err != nil {
			return nil, false, err
		}
		return volume, found, nil
	}
	return nil, false, nil
}

func artifactsPath(outputConfig atc.TaskOutputConfig, artifactsRoot string) string {
	outputSrc := outputConfig.Path
	if len(outputSrc) == 0 {
		outputSrc = outputConfig.Name
	}

	return path.Join(artifactsRoot, outputSrc) + "/"
}

func (src *containerSource) initialize() error {
	return createContainerDir(src.container, artifactsPath(src.outputConfig, src.artifactsRoot))
}

func createContainerDir(container garden.Container, dir string) error {
	emptyTar := new(bytes.Buffer)

	err := tar.NewWriter(emptyTar).Close()
	if err != nil {
		return err
	}

	err = container.StreamIn(garden.StreamInSpec{
		Path:      dir,
		TarStream: emptyTar,
	})
	if err != nil {
		return err
	}

	return nil
}
