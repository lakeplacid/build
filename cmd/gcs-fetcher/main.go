/*
Copyright 2018 Google, Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package main

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/golang/glog"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
)

const (
	defaultWorkers    = 200
	defaultRetries    = 3
	defaultBackoff    = 100 * time.Millisecond
	backoffMultiplier = 2
	stagingFolder     = ".download/"
	userAgent         = "gae-fetcher"
)

var (
	sourceType = flag.String("type", "", "Type of source; Archive or Manifest")
	location   = flag.String("location", "", "Location in GCS of the object; must start with gs://")

	destDir     = flag.String("dest_dir", "", "The root where to write the files.")
	workerCount = flag.Int("workers", defaultWorkers, "The number of files to fetch in parallel.")
	verbose     = flag.Bool("verbose", false, "If true, additional output is logged.")
	retries     = flag.Int("retries", defaultRetries, "Number of times to retry a failed GCS download.")
	backoff     = flag.Duration("backoff", defaultBackoff, "Time to wait when retrying, will be doubled on each retry.")
	timeoutGCS  = flag.Bool("timeout_gcs", true, "If true, a timeout will be used to avoid GCS longtails.")

	// sourceExt is a best-effort to identify files that should have a short
	// download time and thus get a short timeout for the first tries.
	sourceExt = map[string]bool{
		".js":   true,
		".py":   true,
		".php":  true,
		".java": true,
		".go":   true,
		".cs":   true,
		".rb":   true,
		".css":  true,
		".vb":   true,
		".pl":   true, // Perl. C'mon, live a little.
	}
	sourceTimeout = map[int]time.Duration{ // try number -> timeout duration
		0: 1 * time.Second,
		1: 2 * time.Second,
	}
	notSourceTimeout = map[int]time.Duration{ // try number -> timeout duration
		0: 3 * time.Second,
		1: 6 * time.Second,
	}
	defaultTimeout = 1 * time.Hour
	noTimeout      = 0 * time.Second
	errGCSTimeout  = errors.New("GCS timeout")
)

type sizeBytes int64

type sourceInfo struct {
	SourceURL string `json:"sourceUrl"`
	Sha1Sum   string `json:"sha1Sum"`
	// TODO(jasonhall): FileMode
}

type fileMap map[string]sourceInfo

// job is a file to download, corresponds to an entry in the manifest file.
type job struct {
	filename       string
	bucket, object string
	generation     int64
	sha1sum        string
}

// jobAttempt is an attempt to download a particular file, may result in
// success or failure (indicated by err).
type jobAttempt struct {
	started    time.Time
	duration   time.Duration
	err        error
	gcsTimeout time.Duration
}

// jobReport stores all the details about the attempts to download a
// particular file.
type jobReport struct {
	job       job
	started   time.Time
	completed time.Time
	size      sizeBytes
	attempts  []jobAttempt
	success   bool
	finalname string
	err       error
}

type fetchOnceResult struct {
	size sizeBytes
	err  error
}

type stats struct {
	workers     int
	files       int
	size        sizeBytes
	duration    time.Duration
	retries     int
	gcsTimeouts int
	success     bool
	errs        []error
}

// osProvider allows us to inject dependencies to facilitate testing.
type osProvider interface {
	rename(oldpath, newpath string) error
	chmod(name string, mode os.FileMode) error
	create(name string) (*os.File, error)
	mkdirAll(path string, perm os.FileMode) error
	open(name string) (*os.File, error)
	removeAll(path string) error
}

// realOSProvider merely wraps the os package implementations.
type realOSProvider struct{}

func (realOSProvider) rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

func (realOSProvider) chmod(name string, mode os.FileMode) error {
	return os.Chmod(name, mode)
}

func (realOSProvider) create(name string) (*os.File, error) {
	return os.Create(name)
}

func (realOSProvider) mkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (realOSProvider) open(name string) (*os.File, error) {
	return os.Open(name)
}
func (realOSProvider) removeAll(path string) error {
	return os.RemoveAll(path)
}

// gcsProvider allows us to inject dependencies to facilitate testing.
type gcsProvider interface {
	newReader(ctx context.Context, bucket, object string) (io.ReadCloser, error)
}

// realGCSProvider is a wrapper over the GCS client functions.
type realGCSProvider struct {
	client *storage.Client
}

func (gp realGCSProvider) newReader(ctx context.Context, bucket, object string) (io.ReadCloser, error) {
	return gp.client.Bucket(bucket).Object(object).NewReader(ctx)
}

// gcsFetcher is the main workhorse of this package and does all the heavy lifting.
type gcsFetcher struct {
	gcs gcsProvider
	os  osProvider

	destDir    string
	stagingDir string

	// mu guards createdDirs
	mu          sync.Mutex
	createdDirs map[string]bool

	bucket, object string
	generation     int64

	timeoutGCS  bool
	workerCount int
	retries     int
	backoff     time.Duration
}

func (gf *gcsFetcher) recordFailure(j job, started time.Time, gcsTimeout time.Duration, err error, report *jobReport) {
	attempt := jobAttempt{
		started:    started,
		duration:   time.Since(started),
		err:        err,
		gcsTimeout: gcsTimeout,
	}
	report.success = false
	report.err = err // Hold the latest error.
	report.attempts = append(report.attempts, attempt)

	isLast := len(report.attempts) == gf.retries
	if *verbose || isLast {
		retryMsg := ", will retry"
		if isLast {
			retryMsg = ", will no longer retry"
		}
		glog.Infof("Failed to fetch %s%s: %v", formatGCSName(j.bucket, j.object, j.generation), retryMsg, err)
	}
}

func (gf *gcsFetcher) recordSuccess(j job, started time.Time, size sizeBytes, finalname string, report *jobReport) {
	attempt := jobAttempt{
		started:  started,
		duration: time.Since(started),
	}
	report.success = true
	report.err = nil
	report.size = size
	report.attempts = append(report.attempts, attempt)
	report.finalname = finalname

	mibps := math.MaxFloat64
	if attempt.duration > 0 {
		mibps = (float64(report.size) / 1024 / 1024) / attempt.duration.Seconds()
	}
	glog.Infof("Fetched %s (%dB in %v, %.2fMiB/s)", formatGCSName(j.bucket, j.object, j.generation), report.size, attempt.duration, mibps)
}

// fetchObject is responsible for trying (and retrying) to fetch a single file
// from GCS. It first downloads the file to a temp file, then renames it to
// the final location and sets the permissions on the final file.
func (gf *gcsFetcher) fetchObject(ctx context.Context, j job) *jobReport {
	report := &jobReport{job: j, started: time.Now()}
	defer func() {
		report.completed = time.Now()
	}()

	var tmpfile string
	var backoff time.Duration

	for retrynum := 0; retrynum <= gf.retries; retrynum++ {

		// Apply appropriate retry backoff.
		if retrynum > 0 {
			if retrynum == 1 {
				backoff = gf.backoff
			} else {
				backoff *= backoffMultiplier
			}
			time.Sleep(backoff)
		}

		started := time.Now()

		// Download to temp location [destDir]/[stagingDir]/[bucket]-[object]-[retry]
		// If fetchObjectOnceWithTimeout() times out, this file will be orphaned and we can
		// clean it up later.
		tmpfile = filepath.Join(gf.stagingDir, fmt.Sprintf("%s-%s-%d", j.bucket, j.object, retrynum))
		if err := gf.ensureFolders(tmpfile); err != nil {
			e := fmt.Errorf("creating folders for temp file %q: %v", tmpfile, err)
			gf.recordFailure(j, started, noTimeout, e, report)
			continue
		}

		allowedGCSTimeout := gf.timeout(j.filename, retrynum)
		size, err := gf.fetchObjectOnceWithTimeout(ctx, j, allowedGCSTimeout, tmpfile)
		if err != nil {
			e := fmt.Errorf("fetching %q with timeout %v to temp file %q: %v", formatGCSName(j.bucket, j.object, j.generation), allowedGCSTimeout, tmpfile, err)
			gf.recordFailure(j, started, allowedGCSTimeout, e, report)
			continue
		}

		// Rename the temp file to the final filename
		finalname := filepath.Join(gf.destDir, j.filename)
		if err := gf.ensureFolders(finalname); err != nil {
			e := fmt.Errorf("creating folders for final file %q: %v", finalname, err)
			gf.recordFailure(j, started, noTimeout, e, report)
			continue
		}
		if err := gf.os.rename(tmpfile, finalname); err != nil {
			e := fmt.Errorf("renaming %q to %q: %v", tmpfile, finalname, err)
			gf.recordFailure(j, started, noTimeout, e, report)
			continue
		}

		// TODO(jasonco): make the posix attributes match the source
		// This will only work if the original upload sends the posix
		// attributes to GCS. For now, we'll just give the user full
		// access.
		mode := os.FileMode(0555)
		if err := gf.os.chmod(finalname, mode); err != nil {
			e := fmt.Errorf("chmod %q to %v: %v", finalname, mode, err)
			gf.recordFailure(j, started, noTimeout, e, report)
			continue
		}

		gf.recordSuccess(j, started, size, finalname, report)
		break // Success! No more retries needed.
	}

	return report
}

// fetchObjectOnceWithTimeout is merely mechanics to call fetchObjectOnce(),
// using a circuit breaker pattern to timeout the call if it takes too long.
// GCS has long tail latencies, so we retry with low timeouts on the first
// couple of attempts. On subsequent attempts, we simply wait for a long time.
func (gf *gcsFetcher) fetchObjectOnceWithTimeout(ctx context.Context, j job, timeout time.Duration, dest string) (sizeBytes, error) {
	result := make(chan fetchOnceResult, 1)
	breakerSig := make(chan struct{}, 1)

	// Start the function that we want to timeout if it takes too long.
	go func() {
		result <- gf.fetchObjectOnce(ctx, j, dest, breakerSig)
	}()

	// Wait to see who finshes first: function or timeout
	select {
	case r := <-result:
		return r.size, r.err
	case <-ctx.Done():
		close(breakerSig) // Signal fetchObjectOnce() to cancel
		if ctx.Err() == context.DeadlineExceeded {
			return 0, errGCSTimeout
		}
		return 0, ctx.Err()
	case <-time.After(timeout):
		close(breakerSig) // Signal fetchObjectOnce() to cancel
		return 0, errGCSTimeout
	}
}

// fetchObjectOnce has the responsibility of downloading a file from
// GCS and saving it to the dest location. If it receives a signal on
// breakerSig, it will attempt to return quickly, though it is assumed
// that no one is listening for a response anymore.
func (gf *gcsFetcher) fetchObjectOnce(ctx context.Context, j job, dest string, breakerSig <-chan struct{}) fetchOnceResult {
	var result fetchOnceResult

	r, err := gf.gcs.newReader(ctx, j.bucket, j.object)
	if err != nil {
		result.err = fmt.Errorf("creating GCS reader for %q: %v", formatGCSName(j.bucket, j.object, j.generation), err)
		return result
	}
	defer r.Close()

	// If we're cancelled, just return.
	select {
	case <-breakerSig:
		result.err = errGCSTimeout
		return result
	default:
		// Fallthrough
	}

	f, err := gf.os.create(dest)
	if err != nil {
		result.err = fmt.Errorf("creating destination file %q: %v", dest, err)
		return result
	}
	defer f.Close()

	h := sha1.New()
	n, err := io.Copy(f, io.TeeReader(r, h))
	if err != nil {
		result.err = fmt.Errorf("copying bytes from %q to %q: %v", formatGCSName(j.bucket, j.object, j.generation), dest, err)
		return result
	}

	// If we're cancelled, just return.
	select {
	case <-breakerSig:
		result.err = errGCSTimeout
		return result
	default:
		result.size = sizeBytes(n)
		return result
	}

	// TODO(jasonhall): verify the sha1sum before declaring success
	// if j.SHA1Sum != "" {
	//   sha := string(h.Sum(nil))
	//   if sha != sha1sum {
	// 	  return fmt.Errorf("%s SHA mismatch, got %q, want %q", filename, sha, sha1sum)
	//   }
	// }
}

// ensureFolders takes a full path to a filename and makes sure that
// all the folders leading to the filename exist.
func (gf *gcsFetcher) ensureFolders(filename string) error {
	filedir := filepath.Dir(filename)
	gf.mu.Lock()
	defer gf.mu.Unlock()
	if _, ok := gf.createdDirs[filedir]; !ok {
		if err := gf.os.mkdirAll(filedir, os.FileMode(0777)|os.ModeDir); err != nil {
			return fmt.Errorf("ensuring folders for %q: %v", filedir, err)
		}
		gf.createdDirs[filedir] = true
	}
	return nil
}

// doWork is the worker routine. It listens for jobs, fetches the file,
// and emits a job report. This continues until channel job is closed.
func (gf *gcsFetcher) doWork(ctx context.Context, todo <-chan job, results chan<- jobReport) {
	for j := range todo {
		report := gf.fetchObject(ctx, j)
		if *verbose {
			glog.Infof("Report: %#v", report)
		}
		results <- *report
	}
}

// processJobs is the primary concurrency mechanics for gcsFetcher.
// This method spins up a set of worker goroutines, creates a
// goroutine to send all the jobs to the workers, then waits for
// all the jobs to complete. It also compiles and returns final
// statistics for the jobs.
func (gf *gcsFetcher) processJobs(ctx context.Context, jobs []job) stats {
	workerCount := gf.workerCount
	if len(jobs) < workerCount {
		workerCount = len(jobs)
	}
	todo := make(chan job, workerCount)
	results := make(chan jobReport, workerCount)
	stats := stats{workers: workerCount, files: len(jobs), success: true}

	// Spin up our workers.
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			gf.doWork(ctx, todo, results)
			wg.Done()
		}()
	}

	// Queue the jobs.
	started := time.Now()
	var qwg sync.WaitGroup
	qwg.Add(1)
	go func() {
		for _, j := range jobs {
			todo <- j
		}
		qwg.Done()
	}()

	// Consume the reports.
	failed := false
	for n := 0; n < len(jobs); n++ {
		report := <-results
		if !report.success {
			failed = true
		}
		stats.size += report.size
		lastIndex := len(report.attempts) - 1
		stats.retries += lastIndex // First attempt is not considered a "retry".
		finalAttempt := report.attempts[lastIndex]
		stats.duration += finalAttempt.duration
		if finalAttempt.err != nil {
			stats.errs = append(stats.errs, finalAttempt.err)
		}
		for _, attempt := range report.attempts {
			if attempt.gcsTimeout > noTimeout {
				stats.gcsTimeouts++
			}
		}
	}
	qwg.Wait()
	close(results)
	close(todo)
	wg.Wait()

	if failed {
		stats.success = false
		glog.Fatal("Failed to download at least one file. Cannot continue.")
	}

	stats.duration = time.Since(started)
	return stats
}

// getTimeout returns the GCS timeout that should be used for a given
// filenum on a given retry number. GCS has long tails on occasion, so
// in some cases, it's faster to give up early and retry on a second
// connection.
func (gf *gcsFetcher) timeout(filename string, retrynum int) time.Duration {
	if gf.timeoutGCS == false {
		return defaultTimeout
	}

	// Use short timeouts for source code, longer for non-source
	if sourceExt[filepath.Ext(filename)] {
		if timeout, ok := sourceTimeout[retrynum]; ok {
			return timeout
		}
	} else {
		if timeout, ok := notSourceTimeout[retrynum]; ok {
			return timeout
		}
	}
	return defaultTimeout
}

// fetchFromManifest is used when downloading source based on a manifest file.
// It is responsible for fetching the manifest file, decoding the JSON, and
// assembling the list of jobs to process (i.e., files to download).
func (gf *gcsFetcher) fetchFromManifest(ctx context.Context) error {
	started := time.Now()
	glog.Infof("Fetching manifest %s.", formatGCSName(gf.bucket, gf.object, gf.generation))

	// Download the manifest file from GCS.
	j := job{
		filename:   gf.object,
		bucket:     gf.bucket,
		object:     gf.object,
		generation: gf.generation,
	}
	report := gf.fetchObject(ctx, j)
	if !report.success {
		return fmt.Errorf("failed to download manifest %s: %v", formatGCSName(gf.bucket, gf.object, gf.generation), report.err)
	}

	// Decode the JSON manifest
	manifestFile := filepath.Join(gf.destDir, j.filename)
	r, err := gf.os.open(manifestFile)
	defer r.Close()
	if err != nil {
		return fmt.Errorf("opening manifest file %q: %v", manifestFile, err)
	}
	var files fileMap
	if err := json.NewDecoder(r).Decode(&files); err != nil {
		return fmt.Errorf("decoding JSON from manifest file %q: %v", manifestFile, err)
	}

	// Create the jobs
	var jobs []job
	for filename, info := range files {
		bucket, object, generation, err := parseBucketObject(info.SourceURL)
		if err != nil {
			return fmt.Errorf("parsing bucket/object from %q: %v", info.SourceURL, err)
		}
		j := job{
			filename:   filename,
			bucket:     bucket,
			object:     object,
			generation: generation,
			sha1sum:    info.Sha1Sum,
		}
		jobs = append(jobs, j)
	}

	glog.Infof("Processing %v files.", len(jobs))
	stats := gf.processJobs(ctx, jobs)

	// Final cleanup of failed downloads. We won't miss any files; these vestiges
	// are from go routines that have timed out and would otherwise check their
	// circuit breaker and die. However, we won't wait for these remaining
	// go routines to finish because out goal is to get done as fast as possible!
	if err := gf.os.removeAll(gf.stagingDir); err != nil {
		glog.Infof("Failed to remove staging dir %v, continuing: %v", gf.stagingDir, err)
	}

	// Emit final stats.
	mib := float64(stats.size) / 1024 / 1024
	var mibps float64
	if stats.duration > 0 {
		mibps = mib / stats.duration.Seconds()
	}
	manifestDuration := report.attempts[len(report.attempts)-1].duration
	status := "SUCCESS"
	if !stats.success {
		status = "FAILURE"
	}
	glog.Infof("******************************************************")
	glog.Infof("Status:                      %s", status)
	glog.Infof("Started:                     %s", started.Format(time.RFC3339))
	glog.Infof("Completed:                   %s", time.Now().Format(time.RFC3339))
	glog.Infof("Requested workers: %6d", gf.workerCount)
	glog.Infof("Actual workers:    %6d", stats.workers)
	glog.Infof("Total files:       %6d", stats.files)
	glog.Infof("Total retries:     %6d", stats.retries)
	if gf.timeoutGCS {
		glog.Infof("GCS timeouts:      %6d", stats.gcsTimeouts)
	}
	glog.Infof("MiB downloaded:    %9.2f MiB", mib)
	glog.Infof("MiB/s throughput:  %9.2f MiB/s", mibps)

	glog.Infof("Time for manifest: %9.2f ms", float64(manifestDuration)/float64(time.Millisecond))
	glog.Infof("Total time:        %9.2f s", time.Since(started).Seconds())
	glog.Infof("******************************************************")

	if len(stats.errs) > 0 {
		glog.Infof("Errors (%d):", len(stats.errs))
		for err := range stats.errs {
			glog.Info(err)
		}
		glog.Infof("******************************************************")
		return fmt.Errorf("file fetching failed")
	}
	return nil
}

func (gf *gcsFetcher) copyFileFromZip(file *zip.File) error {
	sourceReader, err := file.Open()
	if err != nil {
		return fmt.Errorf("failed to open source file %q: %v", file.Name, err)
	}
	defer sourceReader.Close()

	targetFile := filepath.Join(gf.destDir, file.Name)
	if err := gf.ensureFolders(targetFile); err != nil {
		return fmt.Errorf("failed to create folders for file %q: %v", targetFile, err)
	}

	targetWriter, err := os.OpenFile(targetFile, os.O_WRONLY|os.O_CREATE, file.Mode())
	if err != nil {
		return fmt.Errorf("failed to open target file %q: %v", targetFile, err)
	}
	defer targetWriter.Close()

	if _, err := io.Copy(targetWriter, sourceReader); err != nil {
		return fmt.Errorf("failed to copy %q to %q: %v", file.Name, targetFile, err)
	}
	return nil
}

// fetchFromZip is used when downloading a single zip of source files. It is
// responsible to fetch the zip file and unzip it into the destination folder.
func (gf *gcsFetcher) fetchFromZip(ctx context.Context) error {
	started := time.Now()
	glog.Infof("Fetching archive %s.", formatGCSName(gf.bucket, gf.object, gf.generation))

	// Download the archive from GCS.
	j := job{
		filename:   gf.object,
		bucket:     gf.bucket,
		object:     gf.object,
		generation: gf.generation,
	}
	report := gf.fetchObject(ctx, j)
	if !report.success {
		return fmt.Errorf("failed to download archive %s: %v", formatGCSName(gf.bucket, gf.object, gf.generation), report.err)
	}

	// Unzip into the destination directory
	unzipStart := time.Now()
	zipfile := filepath.Join(gf.destDir, gf.object)
	zipReader, err := zip.OpenReader(zipfile)
	if err != nil {
		return fmt.Errorf("failed to open archive %s: %v", zipfile, err)
	}
	defer zipReader.Close()

	numFiles := 0
	for _, file := range zipReader.File {
		if file.FileInfo().IsDir() {
			continue
		}

		numFiles++
		if err := gf.copyFileFromZip(file); err != nil {
			return err
		}
	}
	unzipDuration := time.Since(unzipStart)

	// Remove the zip file (best effort only, no harm if this fails).
	if err := os.RemoveAll(zipfile); err != nil {
		glog.Infof("Failed to remove zipfile %s, continuing: %v", zipfile, err)
	}

	// Final cleanup of staging directory, which is only a temporary staging
	// location for downloading the zipfile in this case.
	if err := gf.os.removeAll(gf.stagingDir); err != nil {
		glog.Infof("Failed to remove staging dir %q, continuing: %v", gf.stagingDir, err)
	}

	mib := float64(report.size) / 1024 / 1024
	var mibps float64
	zipfileDuration := report.attempts[len(report.attempts)-1].duration
	if zipfileDuration > 0 {
		mibps = mib / zipfileDuration.Seconds()
	}
	glog.Infof("******************************************************")
	glog.Infof("Status:                      SUCCESS")
	glog.Infof("Started:                     %s", started.Format(time.RFC3339))
	glog.Infof("Completed:                   %s", time.Now().Format(time.RFC3339))
	glog.Infof("Total files:       %6d", numFiles)
	glog.Infof("MiB downloaded:    %9.2f MiB", mib)
	glog.Infof("MiB/s throughput:  %9.2f MiB/s", mibps)
	glog.Infof("Time for zipfile:  %9.2f s", zipfileDuration.Seconds())
	glog.Infof("Time to unzip:     %9.2f s", unzipDuration.Seconds())
	glog.Infof("Total time:        %9.2f s", time.Since(started).Seconds())
	glog.Infof("******************************************************")
	return nil
}

// fetch is the main entry point into gcsFetcher. Based on configuration,
// it pulls source from GCS into the destination directory.
// TODO: Use consts in build_types
func (gf *gcsFetcher) fetch(ctx context.Context) error {
	switch *sourceType {
	case "Manifest":
		if err := gf.fetchFromManifest(ctx); err != nil {
			return err
		}
	case "Archive":
		if err := gf.fetchFromZip(ctx); err != nil {
			return err
		}
	default:
		return fmt.Errorf("misconfigured gcsFetcher, must have either -type as either Manifest or Archive: %v", gf)
	}
	return nil
}

// TODO: Parse generation.
func parseBucketObject(filename string) (bucket, object string, generation int64, err error) {
	switch {
	case strings.HasPrefix(filename, "https://storage.googleapis.com/") || strings.HasPrefix(filename, "http://storage.googleapis.com/"):
		// filename looks like "https://storage.googleapis.com/staging.my-project.appspot.com/3aa080e5e72a610b06033dbfee288483d87cfd61"
		if parts := strings.Split(filename, "/"); len(parts) >= 5 {
			bucket := parts[3]
			object := strings.Join(parts[4:], "/")
			return bucket, object, 0, nil
		}
	case strings.HasPrefix(filename, "gs://"):
		// filename looks like "gs://my-bucket/manifest-20171004T175409.json"
		if parts := strings.Split(filename, "/"); len(parts) >= 4 {
			bucket := parts[2]
			object := strings.Join(parts[3:], "/")
			return bucket, object, 0, nil
		}
	}
	return "", "", 0, fmt.Errorf("cannot parse bucket/object from filename %q", filename)
}

func formatGCSName(bucket, object string, generation int64) string {
	n := fmt.Sprintf("gs://%s/%s", bucket, object)
	if generation > 0 {
		n += fmt.Sprintf("#%d", generation)
	}
	return n
}

func buildHTTPClient(ctx context.Context) (*http.Client, error) {
	hc, err := google.DefaultClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create default client: %v", err)
	}

	ts, err := google.DefaultTokenSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create default token source: %v", err)
	}

	hc.Transport = &oauth2.Transport{
		Base:   http.DefaultTransport,
		Source: ts,
	}

	return hc, nil
}

func main() {
	flag.Parse()

	if *location == "" || *sourceType == "" {
		glog.Fatal("Must specify --location and --type")
	}

	ctx := context.Background()
	hc, err := buildHTTPClient(ctx)
	if err != nil {
		glog.Info(err)
		os.Exit(2)
	}

	client, err := storage.NewClient(ctx, option.WithHTTPClient(hc), option.WithUserAgent(userAgent))
	if err != nil {
		glog.Infof("Failed to create new GCS client: %v", err)
		os.Exit(2)
	}

	bucket, object, generation, err := parseBucketObject(*location)
	if err != nil {
		glog.Fatalf("Failed to parse --location: %v", err)
	}

	gcs := &gcsFetcher{
		gcs:         realGCSProvider{client: client},
		os:          realOSProvider{},
		destDir:     *destDir,
		stagingDir:  filepath.Join(*destDir, stagingFolder),
		createdDirs: make(map[string]bool),
		bucket:      bucket,
		object:      object,
		generation:  generation,
		timeoutGCS:  *timeoutGCS,
		workerCount: *workerCount,
		retries:     *retries,
		backoff:     *backoff,
	}
	if err := gcs.fetch(ctx); err != nil {
		glog.Info(err)
		os.Exit(2)
	}
}
