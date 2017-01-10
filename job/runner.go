package job

import (
	"strings"
	"errors"
	"time"
	"bytes"
	"net/http"
	"os/exec"

	log "github.com/Sirupsen/logrus"
)

type JobRunner struct {
	job  *Job
	meta Metadata

	numberOfAttempts uint
	currentRetries   uint
	currentStat      *JobStat
}

var (
	ErrJobDisabled    = errors.New("Job cannot run, as it is disabled")
	ErrCmdIsEmpty     = errors.New("Job Command is empty.")
	ErrJobTypeInvalid = errors.New("Job Type is not valid.")
)

func (j *JobRunner) Run(cache JobCache) (*JobStat, Metadata, error) {
	j.job.lock.RLock()
	defer j.job.lock.RUnlock()

	j.meta.LastAttemptedRun = time.Now()

	if j.job.Disabled {
		log.Infof("Job %s tried to run, but exited early because its disabled.", j.job.Name)
		return nil, j.meta, ErrJobDisabled
	}

	log.Infof("Job %s running", j.job.Name)

	j.runSetup()

	for {
		var err error
		if j.job.JobType == LocalJob {
			log.Debug("Running local job")
			err = j.LocalRun()
		} else if j.job.JobType == RemoteJob {
			log.Debug("Running remote job")
			err = j.RemoteRun()
		} else {
			err = ErrJobTypeInvalid
		}

		if err != nil {
			// Log Error in Metadata
			// TODO - Error Reporting, email error
			log.Errorf("Run Command got an Error: %s", err)

			j.meta.ErrorCount++
			j.meta.LastError = time.Now()

			// Handle retrying
			if j.shouldRetry() {
				j.currentRetries--
				continue
			}

			j.collectStats(false)

			// TODO: Wrap error into something better.
			return j.currentStat, j.meta, err
		} else {
			break
		}
	}

	log.Infof("%s was successful!", j.job.Name)
	j.meta.SuccessCount++
	j.meta.LastSuccess = time.Now()

	j.collectStats(true)

	// Run Dependent Jobs
	if len(j.job.DependentJobs) != 0 {
		for _, id := range j.job.DependentJobs {
			newJob, err := cache.Get(id)
			if err != nil {
				log.Errorf("Error retrieving dependent job with id of %s", id)
			} else {
				newJob.Run(cache)
			}
		}
	}

	return j.currentStat, j.meta, nil
}

// LocalRun executes the Job's local shell command
func (j *JobRunner) LocalRun() error {
	return j.runCmd()
}

// RemoteRun sends a http request, and checks if the response is valid in time,
func (j *JobRunner) RemoteRun() error {
	// Calculate a response timeout
	timeout := j.responseTimeout()

	httpClient := http.Client{
		Timeout: timeout,
	}

	// Normalize the method passed by the user
	method := strings.ToUpper(j.job.RemoteProperties.Method)
	bodyBuffer := bytes.NewBufferString(j.job.RemoteProperties.Body)
	req, err := http.NewRequest(method, j.job.RemoteProperties.Url, bodyBuffer)
	if err != nil {
		return err
	}

	// Set default or user's passed headers
	j.setHeaders(req)

	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}

	// Check if we got any of the status codes the user asked for
	if j.checkExpected(res.StatusCode) {
		return nil
	} else {
		return errors.New(res.Status)
	}
}

func (j *JobRunner) runCmd() error {
	j.numberOfAttempts++

	// Execute command
	args, err := shParser.Parse(j.job.Command)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return ErrCmdIsEmpty
	}
	cmd := exec.Command(args[0], args[1:]...)
	return cmd.Run()
}

func (j *JobRunner) shouldRetry() bool {
	// Check number of retries left
	if j.currentRetries == 0 {
		return false
	}

	// Check Epsilon
	if j.job.Epsilon != "" {
		if j.job.epsilonDuration.ToDuration() != 0 {
			timeSinceStart := time.Now().Sub(j.job.NextRunAt)
			timeLeftToRetry := j.job.epsilonDuration.ToDuration() - timeSinceStart
			if timeLeftToRetry < 0 {
				return false
			}
		}
	}

	return true
}

func (j *JobRunner) runSetup() {
	// Setup Job Stat
	j.currentStat = NewJobStat(j.job.Id)

	// Init retries
	j.currentRetries = j.job.Retries
}

func (j *JobRunner) collectStats(success bool) {
	j.currentStat.ExecutionDuration = time.Now().Sub(j.currentStat.RanAt)
	j.currentStat.Success = success
	j.currentStat.NumberOfRetries = j.job.Retries - j.currentRetries
}

func (j *JobRunner) checkExpected(statusCode int) bool {
	// If no expected response codes passed, add 200 status code as expected
	if len(j.job.RemoteProperties.ExpectedResponseCodes) == 0 {
		j.job.RemoteProperties.ExpectedResponseCodes = append(j.job.RemoteProperties.ExpectedResponseCodes, 200)
	}
	for _, expected := range j.job.RemoteProperties.ExpectedResponseCodes {
		if expected == statusCode {
			return true
		}
	}

	return false
}

func (j *JobRunner) responseTimeout() time.Duration {
	responseTimeout := j.job.RemoteProperties.Timeout
	if responseTimeout == 0 {

		// set default to 30 seconds
		responseTimeout = 30
	}
	return time.Duration(responseTimeout) * time.Second
}

func (j *JobRunner) setHeaders(req *http.Request) {
	// A valid assumption is that the user is sending something in json cause we're past 2017, check if the user
	// already added it, if not, add it to the header
	if !j.keyInHeaders("Content-Type", j.job.RemoteProperties.Headers) {
		jsonContentType := "application/json"
		req.Header.Set("Content-Type", jsonContentType)

		// Add the new header to the job properties
		j.job.RemoteProperties.Headers = append(j.job.RemoteProperties.Headers, Header{"Content-Type", jsonContentType})
	}

	// Set any custom headers
	for _, header := range j.job.RemoteProperties.Headers {
		req.Header.Set(header.Key, header.Value)
	}
}

func (j *JobRunner) keyInHeaders(keyExpected string, headers []Header) bool {
	for _, header := range headers {
		if header.Key == keyExpected {
			return true
		}
	}
	return false
}
