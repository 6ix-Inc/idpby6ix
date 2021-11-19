package airbyte

import (
	"errors"
	"fmt"
	"github.com/jitsucom/jitsu/server/drivers/base"
	"github.com/jitsucom/jitsu/server/logging"
	"github.com/jitsucom/jitsu/server/parsers"
	"github.com/jitsucom/jitsu/server/runner"
	"github.com/jitsucom/jitsu/server/safego"
	"github.com/jitsucom/jitsu/server/uuid"
	"io"
	"os"
	"os/exec"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const (
	connectionStatusSucceed = "SUCCEEDED"
	connectionStatusFailed  = "FAILED"
)

//Runner is an Airbyte Docker runner
//Can only be used once
//Self-closed (see run() func)
type Runner struct {
	//DockerImage without 'airbyte/' prefix
	DockerImage string
	Version     string

	identifier string
	closed     chan struct{}

	command *exec.Cmd
}

//NewRunner returns configured Airbyte Runner
func NewRunner(dockerImage, imageVersion, identifier string) *Runner {
	if identifier == "" {
		identifier = fmt.Sprintf("%s-%s-%s", dockerImage, imageVersion, uuid.New())
	}
	return &Runner{
		DockerImage: dockerImage,
		Version:     imageVersion,
		identifier:  identifier,
		closed:      make(chan struct{}),
	}
}

//String returns exec command string
func (r *Runner) String() string {
	if r.command == nil {
		return ""
	}

	return r.command.String()
}

//Spec runs airbyte docker spec command and returns spec and err if occurred
func (r *Runner) Spec() (interface{}, error) {
	resultParser := &synchronousParser{desiredRowType: SpecType}
	errWriter := logging.NewStringWriter()

	err := r.run(resultParser.parse, copyTo(errWriter), time.Minute, "run", "--rm", "-i", "--name", r.identifier, fmt.Sprintf("%s:%s", Instance.AddAirbytePrefix(r.DockerImage), r.Version), "spec")
	if err != nil {
		if err == runner.ErrNotReady {
			return nil, err
		}

		errMsg := Instance.BuildMsg("Error loading airbyte spec:", resultParser.output, errWriter, err)
		logging.Error(errMsg)
		return nil, errors.New(errMsg)
	}

	return resultParser.parsedRow, nil
}

func (r *Runner) Check(airbyteSourceConfig interface{}) error {
	resultParser := &synchronousParser{desiredRowType: ConnectionStatusType}
	errWriter := logging.NewStringWriter()

	absoluteDir, relatedFilePath, err := saveConfig(airbyteSourceConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(absoluteDir); err != nil {
			logging.SystemErrorf("Error deleting generated airbyte config dir [%s]: %v", absoluteDir, err)
		}
	}()

	err = r.run(resultParser.parse, copyTo(errWriter), time.Minute,
		"run", "--rm", "-i", "--name", r.identifier, "-v", fmt.Sprintf("%s:%s", Instance.WorkspaceVolume, VolumeAlias), fmt.Sprintf("%s:%s", Instance.AddAirbytePrefix(r.DockerImage), r.Version), "check", "--config", path.Join(VolumeAlias, relatedFilePath))
	if err != nil {
		if err == runner.ErrNotReady {
			return err
		}

		errMsg := Instance.BuildMsg("Error executing airbyte check:", resultParser.output, errWriter, err)
		logging.Error(errMsg)
		return errors.New(errMsg)
	}

	switch resultParser.parsedRow.ConnectionStatus.Status {
	case connectionStatusSucceed:
		return nil
	case connectionStatusFailed:
		return errors.New(resultParser.parsedRow.ConnectionStatus.Message)
	default:
		return fmt.Errorf("unknown airbyte connection status [%s]: %s", resultParser.parsedRow.ConnectionStatus.Status, resultParser.parsedRow.ConnectionStatus.Message)
	}
}

//Discover returns discovered raw catalog
func (r *Runner) Discover(airbyteSourceConfig interface{}, timeout time.Duration) (*CatalogRow, error) {
	resultParser := &synchronousParser{desiredRowType: CatalogType}
	errStrWriter := logging.NewStringWriter()
	dualStdErrWriter := logging.Dual{FileWriter: errStrWriter, Stdout: logging.NewPrefixDateTimeProxy("[discover]", Instance.LogWriter)}

	absoluteDirPath, relatedFilePath, err := saveConfig(airbyteSourceConfig)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := os.RemoveAll(absoluteDirPath); err != nil {
			logging.SystemErrorf("Error deleting generated airbyte config dir [%s]: %v", absoluteDirPath, err)
		}
	}()

	err = r.run(resultParser.parse, copyTo(dualStdErrWriter), timeout,
		"run", "--rm", "-i", "--name", r.identifier, "-v", fmt.Sprintf("%s:%s", Instance.WorkspaceVolume, VolumeAlias), fmt.Sprintf("%s:%s", Instance.AddAirbytePrefix(r.DockerImage), r.Version), "discover", "--config", path.Join(VolumeAlias, relatedFilePath))
	if err != nil {
		if err == runner.ErrNotReady {
			return nil, err
		}

		errMsg := Instance.BuildMsg("Error loading airbyte catalog:", resultParser.output, errStrWriter, err)
		logging.Error(errMsg)
		return nil, errors.New(errMsg)
	}

	return resultParser.parsedRow.Catalog, nil
}

func (r *Runner) Read(dataConsumer base.CLIDataConsumer, streamsRepresentation map[string]*base.StreamRepresentation, taskLogger logging.TaskLogger, taskCloser base.CLITaskCloser, sourceID, statePath string) error {
	asyncParser := &asynchronousParser{
		dataConsumer:          dataConsumer,
		streamsRepresentation: streamsRepresentation,
		logger:                taskLogger,
	}

	stdoutHandler := func(stdout io.Reader) error {
		defer func() {
			if rec := recover(); rec != nil {
				logging.Error("panic in airbyte runner")
				logging.Error(string(debug.Stack()))
				msg := fmt.Sprintf("%v. Process will be killed", rec)
				taskCloser.CloseWithError(msg, true)
				if killErr := r.Close(); killErr != nil && killErr != runner.ErrAirbyteAlreadyTerminated {
					taskLogger.ERROR("Error closing airbyte runner: %v", killErr)
					logging.Errorf("[%s] closing airbyte runner: %v", taskCloser.TaskID(), killErr)
				}
			}
		}()

		err := asyncParser.parse(stdout)
		if err != nil {
			taskCloser.CloseWithError(fmt.Sprintf("Process error: %v. Process will be killed", err), false)
			if killErr := r.Close(); killErr != nil && killErr != runner.ErrAirbyteAlreadyTerminated {
				taskLogger.ERROR("Error closing airbyte runner: %v", killErr)
				logging.Errorf("[%s] closing airbyte runner: %v", taskCloser.TaskID(), killErr)
			}

			return err
		}
		return nil
	}

	dualStdErrWriter := logging.Dual{FileWriter: taskLogger, Stdout: logging.NewPrefixDateTimeProxy(fmt.Sprintf("[%s]", sourceID), Instance.LogWriter)}

	args := []string{"run", "--rm", "-i", "--name", taskCloser.TaskID(), "-v", fmt.Sprintf("%s:%s", Instance.WorkspaceVolume, VolumeAlias), fmt.Sprintf("%s:%s", Instance.AddAirbytePrefix(r.DockerImage), r.Version), "read", "--config", path.Join(VolumeAlias, sourceID, r.DockerImage, base.ConfigFileName), "--catalog", path.Join(VolumeAlias, sourceID, r.DockerImage, base.CatalogFileName)}

	if statePath != "" {
		args = append(args, "--state", path.Join(VolumeAlias, sourceID, r.DockerImage, base.StateFileName))
	}

	taskLogger.INFO("ID [%s] exec: %s %s", r.identifier, DockerCommand, strings.Join(args, " "))
	return r.run(stdoutHandler, copyTo(dualStdErrWriter), time.Hour*24, args...)
}

func (r *Runner) Close() error {
	if r.terminated() {
		return runner.ErrAirbyteAlreadyTerminated
	}

	close(r.closed)

	exec.Command("docker", "stop", r.identifier, "&").Start()

	return r.command.Process.Kill()
}

func (r *Runner) terminated() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
}

func (r *Runner) run(stdoutHandler, stderrHandler func(io.Reader) error, timeout time.Duration, args ...string) error {
	if r.terminated() {
		return runner.ErrAirbyteAlreadyTerminated
	}

	if !Instance.IsImagePulled(Instance.AddAirbytePrefix(r.DockerImage), r.Version) {
		return runner.ErrNotReady
	}

	//self closed
	safego.Run(func() {
		ticker := time.NewTicker(timeout)
		for {
			select {
			case <-r.closed:
				return
			case <-ticker.C:
				logging.Warnf("[%s] Airbyte run timeout after [%s]", r.identifier, timeout.String())
				if err := r.Close(); err != nil {
					if err != runner.ErrAirbyteAlreadyTerminated {
						logging.SystemErrorf("Error terminating Airbyte runner [%s:%s] after timeout: %v", r.DockerImage, r.Version, err)
					}
				}
			}
		}
	})

	defer r.Close()

	//exec cmd and analyze response from stdout & stderr
	r.command = exec.Command(DockerCommand, args...)
	stdout, _ := r.command.StdoutPipe()
	defer stdout.Close()
	stderr, _ := r.command.StderrPipe()
	defer stderr.Close()

	err := r.command.Start()
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	var parsingErr error
	//writing result to stdout
	wg.Add(1)
	safego.Run(func() {
		defer wg.Done()
		parsingErr = stdoutHandler(stdout)
	})

	//writing process logs to stderr
	wg.Add(1)
	safego.Run(func() {
		defer wg.Done()
		if readingErr := stderrHandler(stderr); readingErr != nil {
			logging.SystemErrorf("Error reading airbyte stderr: %v", readingErr)
		}
	})

	wg.Wait()

	err = r.command.Wait()
	if err != nil {
		return err
	}

	if parsingErr != nil {
		return parsingErr
	}

	return nil
}

func copyTo(writer io.Writer) func(r io.Reader) error {
	return func(r io.Reader) error {
		if _, err := io.Copy(writer, r); err != nil {
			return fmt.Errorf("error reading: %v", err)
		}

		return nil
	}
}

//saveConfig saves config as file for mounting
//returns absolute dir and related file path (generated dir + generated file) and err if occurred
func saveConfig(airbyteSourceConfig interface{}) (string, string, error) {
	dirName := uuid.NewLettersNumbers()
	fileName := uuid.NewLettersNumbers() + ".json"

	absoluteDirPath := path.Join(Instance.ConfigDir, dirName)
	if err := logging.EnsureDir(absoluteDirPath); err != nil {
		return "", "", fmt.Errorf("Error creating airbyte generated config dir: %v", err)
	}

	absoluteFilePath := path.Join(absoluteDirPath, fileName)
	//write airbyte config as file path
	_, err := parsers.ParseJSONAsFile(absoluteFilePath, airbyteSourceConfig)
	if err != nil {
		return "", "", fmt.Errorf("Error writing airbyte config [%v]: %v", airbyteSourceConfig, err)
	}

	return absoluteDirPath, path.Join(dirName, fileName), nil
}