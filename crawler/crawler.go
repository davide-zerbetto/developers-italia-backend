package crawler

import (
	"os"
	"sync"

	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/italia/developers-italia-backend/httpclient"
	"github.com/italia/developers-italia-backend/metrics"

	"github.com/italia/developers-italia-backend/publiccode"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Repository is a single code repository.
type Repository struct {
	Name       string
	FileRawURL string
	Domain     string
	Headers    map[string]string
}

type Handler func(domain Domain, url string, repositories chan Repository, wg *sync.WaitGroup) (string, error)

// Process delegates the work to single domain crawlers.
func ProcessDomain(domain Domain, repositories chan Repository, wg *sync.WaitGroup) {
	// Redis connection.
	redisClient, err := RedisClientFactory(viper.GetString("REDIS_URL"))
	if err != nil {
		log.Error(err)
	}

	// Base starting URL.
	url := domain.URL
	for {
		// Set the value of nextURL on redis to "failed".
		err = redisClient.HSet(domain.Id, url, "failed").Err()
		if err != nil {
			log.Error(err)
		}

		nextURL, err := domain.processAndGetNextURL(url, wg, repositories)
		if err != nil {
			log.Errorf("error reading %s repository list: %v. NextUrl: %v", url, err, nextURL)
			log.Errorf("Retry: %s", nextURL)
			nextURL = url
		}
		// If reached, the repository list was successfully retrieved.
		// Delete the repository url from redis.
		err = redisClient.HDel(domain.Id, url).Err()
		if err != nil {
			log.Error(err)
		}

		// If end is reached, nextUrl is empty.
		if nextURL == "" {
			log.Infof("Url: %s - is the last one.", url)
			wg.Done()
			return
		}
		// Update url to nextURL.
		url = nextURL
	}
}

func ProcessRepositories(repositories chan Repository, wg *sync.WaitGroup) {
	log.Debug("Repositories are going to be processed...")

	// Init Prometheus for metrics.
	metrics.RegisterPrometheusCounter("repository_processed", "Number of repository processed.")
	metrics.RegisterPrometheusCounter("repository_file_saved", "Number of file saved.")
	metrics.RegisterPrometheusCounter("repository_file_saved_valid", "Number of valid file saved.")

	for repository := range repositories {
		wg.Add(1)
		go checkAvailability(repository, wg)
	}

}

func checkAvailability(repository Repository, wg *sync.WaitGroup) {
	name := repository.Name
	fileRawUrl := repository.FileRawURL
	domain := repository.Domain
	headers := repository.Headers

	metrics.GetCounter("repository_processed").Inc()
	metrics.GetCounter(repository.Domain).Inc()

	resp, err := httpclient.GetURL(fileRawUrl, headers)
	// If it's available and no error returned.
	if resp.Status.Code == http.StatusOK && err == nil {
		// Save the file.
		saveFile(domain, name, resp.Body)

		// Validate file.
		err := validateRemoteFile(resp.Body, fileRawUrl)
		if err != nil {
			log.Warn("Validator fails for: " + fileRawUrl)
			log.Warn("Validator errors:" + err.Error())
		}
	}

	// Defer waiting group close.
	wg.Done()
}

// saveFile save the chosen <file_name> in ./data/<source>/<vendor>/<repo>/<file_name>
func saveFile(source, name string, data []byte) {
	fileName := viper.GetString("CRAWLED_FILENAME")
	vendor, repo := splitFullName(name)

	path := filepath.Join("./data", source, vendor, repo)

	// MkdirAll will create all the folder path, if not exists.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.MkdirAll(path, os.ModePerm)
	}

	err := ioutil.WriteFile(filepath.Join(path, fileName), data, 0644)
	if err != nil {
		log.Error(err)
	}

	// Update counter for file saved.
	metrics.GetCounter("repository_file_saved").Inc()
}

// splitFullName split a git FullName format to vendor and repo strings.
func splitFullName(fullName string) (string, string) {
	s := strings.Split(fullName, "/")
	return s[0], s[1]
}

// validateRemoteFile save the chosen <file_name> in ./data/<source>/<vendor>/<repo>/<file_name>
func validateRemoteFile(data []byte, url string) error {
	fileName := viper.GetString("CRAWLED_FILENAME")
	// Parse data into pc struct and validate.
	baseURL := strings.TrimSuffix(url, fileName)
	// Set remore URL for remote validation (it will check files availability).
	publiccode.BaseDir = baseURL
	var pc publiccode.PublicCode

	err := publiccode.Parse(data, &pc)

	if err != nil {
		return err
	}

	metrics.GetCounter("repository_file_saved_valid").Inc()
	return err

}

// WaitingLoop waits until all the goroutines counter is zero and close the repositories channel.
func WaitingLoop(repositories chan Repository, wg *sync.WaitGroup) {
	wg.Wait()
	close(repositories)
}
