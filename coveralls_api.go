package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
)

// processParallelFinish notifies coveralls that all jobs are completed
// ref. https://docs.coveralls.io/parallel-build-webhook
func processParallelFinish(jobID, token string) error {
	var name string
	if reponame != nil && *reponame != "" {
		name = *reponame
	} else if s := os.Getenv("GITHUB_REPOSITORY"); s != "" {
		name = s
	}

	params := make(url.Values)
	params.Set("repo_token", token)
	params.Set("repo_name", name)
	params.Set("payload[build_num]", jobID)
	params.Set("payload[status]", "done")
	res, err := http.PostForm(*endpoint+"/webhook", params)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	bodyBytes, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("Unable to read response body from coveralls: %s", err)
	}

	if res.StatusCode >= http.StatusInternalServerError && *shallow {
		fmt.Println("coveralls server failed internally")
		return nil
	}

	if res.StatusCode != 200 {
		return fmt.Errorf("Bad response status from coveralls: %d\n%s", res.StatusCode, bodyBytes)
	}

	var response WebHookResponse
	if err = json.Unmarshal(bodyBytes, &response); err != nil {
		return fmt.Errorf("Unable to unmarshal response JSON from coveralls: %s\n%s", err, bodyBytes)
	}

	if !response.Done {
		return fmt.Errorf("jobs are not completed:\n%s", bodyBytes)
	}

	return nil
}

func submitCoverallsJob(j Job) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}

	params := make(url.Values)
	params.Set("json", string(b))
	res, err := http.PostForm(*endpoint+"/api/v1/jobs", params)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	bodyBytes, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("Unable to read response body from coveralls: %s", err)
	}

	if res.StatusCode >= http.StatusInternalServerError && *shallow {
		fmt.Println("coveralls server failed internally")
		return nil
	}

	if res.StatusCode != 200 {
		return fmt.Errorf("Bad response status from coveralls: %d\n%s", res.StatusCode, bodyBytes)
	}
	var response Response
	if err = json.Unmarshal(bodyBytes, &response); err != nil {
		return fmt.Errorf("Unable to unmarshal response JSON from coveralls: %s\n%s", err, bodyBytes)
	}
	if response.Error {
		return errors.New(response.Message)
	}
	fmt.Println(response.Message)
	fmt.Println(response.URL)
	return nil
}
