package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/go-playground/validator"
	vcnAPI "github.com/vchain-us/vcn/pkg/api"
	vcnFileExtractor "github.com/vchain-us/vcn/pkg/extractor/file"
	vcnMeta "github.com/vchain-us/vcn/pkg/meta"
	vcnStore "github.com/vchain-us/vcn/pkg/store"
	vcnURI "github.com/vchain-us/vcn/pkg/uri"
)

const (
	red    = "\033[1;31m%s\033[0m"
	green  = "\033[1;32m%s\033[0m"
	yellow = "\033[1;33m%s\033[0m"
)

var (
	errAPIKeyNotFound = errors.New("API key not found")
)

type GitHubReleaseAuthor struct {
	Login string `json:"login" validate:"required"`
}

type GitHubReleaseAssetUploader struct {
	Login string `json:"login" validate:"required"`
}

type GitHubReleaseAsset struct {
	URL      string                      `json:"url" validate:"required"`
	Name     string                      `json:"name" validate:"required"`
	Uploader *GitHubReleaseAssetUploader `json:"uploader" validate:"required"`
}

type GitHubRelease struct {
	TarballURL string                `json:"tarball_url" validate:"required"`
	ZipballURL string                `json:"zipball_url" validate:"required"`
	TagName    string                `json:"tag_name" validate:"required"`
	Author     *GitHubReleaseAuthor  `json:"author" validate:"required"`
	Assets     []*GitHubReleaseAsset `json:"assets"`
}

func main() {
	// validate number of inputs
	expectedNbArgs := 8
	if len(os.Args)-1 != expectedNbArgs {
		fmt.Printf(red, fmt.Sprintf(
			"invalid args %v: expecting %d arguments values, got %d\n",
			os.Args, expectedNbArgs, len(os.Args)-1))
		os.Exit(1)
	}

	// validate inputs
	cnilURL := strings.TrimSuffix(getArg(1, "CNIL REST API URL", true), "/")
	cnilToken := getArg(2, "CNIL REST API personal token", true)
	cnilHost := getArg(3, "CNIL gRPC API host", true)
	cnilPort := getArg(4, "CNIL gRPC API port", true)
	cnilNoTLS := getArg(5, "CNIL gRPC no TLS", false)
	ledgerID := getArg(6, "CNIL ledger ID", true)
	releaseURL := getArg(7, "Release URL", true)
	githubToken := getArg(8, "GitHub token", false)
	fmt.Println()

	var err error
	var noTLS bool
	if len(cnilNoTLS) > 0 {
		noTLS, err = strconv.ParseBool(cnilNoTLS)
		if err != nil {
			fmt.Print(red, fmt.Sprintf(
				"ABORTING: error parsing the \"no TLS\" argument value \"%s\": %v\n",
				cnilNoTLS, err))
			os.Exit(1)
		}
	}

	// reusable HTTP client
	httpClient := &http.Client{Timeout: 30 * time.Second}

	// get the release
	var release GitHubRelease
	if err := getRelease(httpClient, releaseURL, githubToken, &release); err != nil {
		fmt.Print(red, fmt.Sprintf("ABORTING: %v\n", err))
		os.Exit(1)
	}

	// merge source codes archives with assets and treat them all as assets
	// assumes zipball URLs start like this:
	// https://api.github.com/repos/<owner>/<repo-name>/...
	repoName := strings.Split(release.ZipballURL, "/")[5]
	repoAndTag := repoName + "-" + release.TagName
	assetsNames := []string{repoAndTag + ".zip", repoAndTag + ".tar.gz"}
	assetsURLs := []string{release.ZipballURL, release.TarballURL}
	releaseAuthorSignerID := release.Author.Login + "@github"
	signerIDs := []string{releaseAuthorSignerID, releaseAuthorSignerID}
	for _, asset := range release.Assets {
		assetsNames = append(assetsNames, asset.Name)
		assetsURLs = append(assetsURLs, asset.URL)
		signerIDs = append(signerIDs, asset.Uploader.Login+"@github")
	}

	// create temporary dir for storing downloaded assets
	tmpDir, _ := filepath.Abs("notarize-release-assets")
	if err := os.Mkdir(tmpDir, os.ModePerm); err != nil {
		fmt.Printf(red, fmt.Sprintf(
			"ABORTING: error creating temp dir for storing downloaded assets: %v\n",
			err))
		os.Exit(1)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			fmt.Printf(red, fmt.Sprintf("error deleting temp dir %s: %v\n", tmpDir, err))
		}
	}()

	// download assets
	assetsFiles, err := downloadAssets(httpClient, tmpDir, assetsURLs, assetsNames, githubToken)
	if err != nil {
		fmt.Printf(red, fmt.Sprintf("ABORTING: %v", err))
		os.Exit(1)
	}

	fmt.Printf("\nNotarizing %d release assets ...\n\n", len(assetsFiles))

	// make sure the local VCN store directory exists
	options := &vcnOptions{storeDir: "./.vcn", cnilHost: cnilHost, cnilPort: cnilPort}
	if err := os.MkdirAll(options.storeDir, os.ModePerm); err != nil {
		fmt.Printf(red, fmt.Sprintf(
			"ABORTING: error creating local vcn store directory %s: %v\n", options.storeDir, err))
		os.Exit(1)
	}
	// initialize VCN store
	vcnStore.SetDir(options.storeDir)
	vcnStore.LoadConfig()

	// get and rotate or create API keys for each (unique) signer ID
	cnilAPIOptions := &cnilOptions{baseURL: cnilURL, token: cnilToken, ledgerID: ledgerID}
	apiKeys, err := getAndRotateOrCreateAPIKeys(httpClient, cnilAPIOptions, signerIDs)
	if err != nil {
		fmt.Printf(red, fmt.Sprintf("ABORTING: %v\n", err))
		os.Exit(1)
	}

	// create and connect the vcn clients
	vcnUsers := make([]*vcnAPI.LcUser, 0, len(apiKeys))
	vcnUsersPerAPIKey := make(map[string]*vcnAPI.LcUser)

	defer func() {
		for _, vcnUser := range vcnUsersPerAPIKey {
			if err := vcnUser.Client.Disconnect(); err != nil {
				fmt.Printf(red, fmt.Sprintf("error disconnecting vcn client: %v\n", err))
			}
		}
	}()

	for _, apiKey := range apiKeys {
		if vcnUser, ok := vcnUsersPerAPIKey[apiKey]; ok {
			vcnUsers = append(vcnUsers, vcnUser)
			continue
		}
		options.cnilAPIKey = apiKey
		vcnUser, err := vcnAPI.NewLcUser(
			options.cnilAPIKey, "", options.cnilHost, options.cnilPort, "", false, noTLS)
		if err != nil {
			fmt.Printf(red, fmt.Sprintf("ABORTING: error initializing vcn client: %v\n", err))
			os.Exit(1)
		}
		if err := vcnUser.Client.Connect(); err != nil {
			fmt.Printf(red, fmt.Sprintf("ABORTING: error connecting vcn client: %v\n", err))
			os.Exit(1)
		}
		vcnUsersPerAPIKey[apiKey] = vcnUser
		vcnUsers = append(vcnUsers, vcnUser)
	}

	// notarize each asset
	for i, assetFile := range assetsFiles {
		// create VCN artifact from asset file
		artifact, err := vcnArtifactFromAssetFile(assetFile)
		if err != nil {
			fmt.Printf(red, fmt.Sprintf("ABORTING: %v\n", err))
			os.Exit(1)
		}

		// notarize the asset file
		fmt.Printf("Notarizing asset %s ...\n", artifact.Name)
		notarizedArtifact, err := notarizeAndVerify(vcnUsers[i], artifact, options)
		if err != nil {
			fmt.Printf(red, fmt.Sprintf("ABORTING: %v\n", err))
			os.Exit(1)
		}

		notarizedArtifactDetails := fmt.Sprintf(`
	Name:         %s
	Hash:         %s
	Size:         %s
	Timestamp:    %s
	ContentType:  %s
	SignerID:     %s
	Status:       %s
`,
			notarizedArtifact.Name,
			notarizedArtifact.Hash,
			humanize.Bytes(notarizedArtifact.Size),
			notarizedArtifact.Timestamp.Format(time.UnixDate),
			notarizedArtifact.ContentType,
			notarizedArtifact.Signer,
			coloredStatus(notarizedArtifact.Status))

		fmt.Printf(green,
			fmt.Sprintf("Successfully notarized asset %s: %s\n", artifact.Name, notarizedArtifactDetails))
	}

	// print success message
	fmt.Printf(green, fmt.Sprintf(
		"All %d release assets have been successfully notarized.\n", len(assetsFiles)))
}

func getArg(argIndex int, argName string, required bool) string {
	argVal := strings.TrimSpace(os.Args[argIndex])
	fmt.Printf("  - %s: %s (length: %d)\n", argName, argVal, len(argVal))
	if required && len(argVal) == 0 {
		fmt.Printf(red, fmt.Sprintf(
			"ABORTING: required argument %s value is empty\n", argName))
		os.Exit(1)
	}
	return argVal
}

func getRelease(
	httpClient *http.Client,
	releaseURL string,
	githubToken string,
	release *GitHubRelease,
) error {

	req, err := http.NewRequest("GET", releaseURL, nil)
	if err != nil {
		return fmt.Errorf(
			"error creating new HTTP GET %s request for getting the release details: %v",
			releaseURL, err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if len(githubToken) > 0 {
		req.Header.Set("Authorization", "token "+githubToken)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error getting the release details from URL %s: %v", releaseURL, err)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf(
			"error getting the release details from URL %s: error reading response body: %v",
			releaseURL, err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf(
			"error getting the release details from URL %s: expected a 2xx HTTP code, got %d with body %s",
			releaseURL, resp.StatusCode, respBody)
	}

	if err := json.Unmarshal(respBody, release); err != nil {
		return fmt.Errorf(
			"error getting the release details from URL %s: error JSON-unmarshaling the response body %s: %v",
			releaseURL, respBody, err)
	}

	if err := validator.New().Struct(release); err != nil {
		return fmt.Errorf("validation of the release details failed: %v", err)
	}

	return nil
}

func downloadAssets(
	httpClient *http.Client,
	dir string,
	urls []string,
	assetsNames []string,
	githubToken string,
) ([]string, error) {

	var filePaths []string
	var files []*os.File
	bodies := make(map[string]io.ReadCloser)

	defer func() {
		for _, f := range files {
			if err := f.Close(); err != nil {
				fmt.Printf(red, fmt.Sprintf(
					"error deleting asset temp file %s: %v\n",
					filepath.Join(dir, f.Name()), err))
			}
		}
		for a, b := range bodies {
			if err := b.Close(); err != nil {
				fmt.Printf(red, fmt.Sprintf(
					"error closing HTTP response body after downloading asset %s: %v\n",
					a, err))
			}
		}
	}()

	for i, u := range urls {
		u = strings.TrimSpace(u)
		if len(u) == 0 {
			return nil, fmt.Errorf(
				"empty asset download URL found in the list of passed URLs '%v'", urls)
		}

		fileName := assetsNames[i]
		filePath := filepath.Join(dir, fileName)

		fmt.Printf("Downloading asset %s to temp file %s ...\n", u, filePath)
		file, err := os.Create(filePath)
		if err != nil {
			return nil, fmt.Errorf("error creating temp file %s", filePath)
		}
		files = append(files, file)

		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf(
				"error creating new HTTP GET %s request for downloading asset: %v", u, err)
		}
		if !strings.Contains(u, "zipball") && !strings.Contains(u, "tarball") {
			req.Header.Set("Accept", "application/octet-stream")
		}
		if len(githubToken) > 0 {
			req.Header.Set("Authorization", "token "+githubToken)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("error downloading asset from URL %s: %v", u, err)
		}
		bodies[fileName] = resp.Body
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf(
				"error downloading asset from URL %s: expected a 2xx HTTP code, got %d",
				u, resp.StatusCode)
		}

		if _, err := io.Copy(file, resp.Body); err != nil {
			return nil, fmt.Errorf(
				"error saving downloaded asset %s to temp file %s: %v",
				fileName, filePath, err)
		}

		filePaths = append(filePaths, filePath)
	}

	return filePaths, nil
}

type cnilOptions struct {
	baseURL  string
	token    string
	ledgerID string
}

func getAndRotateOrCreateAPIKeys(
	httpClient *http.Client,
	options *cnilOptions,
	signerIDs []string,
) (apiKeys []string, err error) {

	apiKeys = make([]string, 0, len(signerIDs))
	apiKeysPerSignerID := make(map[string]string)

	for _, signerID := range signerIDs {
		if apiKey, ok := apiKeysPerSignerID[signerID]; ok {
			apiKeys = append(apiKeys, apiKey)
			continue
		}

		var apiKeyResp *APIKeyResponse
		apiKeyResp, err = getAPIKey(httpClient, options, signerID)
		if errors.Is(err, errAPIKeyNotFound) {
			apiKeyResp, err = createAPIKey(httpClient, options, signerID)
		} else if err == nil {
			apiKeyResp, err = rotateAPIKey(httpClient, options, apiKeyResp.ID)
		}

		if err != nil {
			err = fmt.Errorf(
				"error getting or creating / rotating API key for signer ID %s: %v",
				signerID, err)
			return
		}

		apiKeysPerSignerID[signerID] = apiKeyResp.Key
		apiKeys = append(apiKeys, apiKeyResp.Key)
	}

	return
}

type APIKeyResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type APIKeysPageResponse struct {
	Total uint64            `json:"total"`
	Items []*APIKeyResponse `json:"items"`
}

func getAPIKey(
	httpClient *http.Client,
	options *cnilOptions,
	signerID string,
) (*APIKeyResponse, error) {
	url := fmt.Sprintf(
		"%s/api_keys/identity/%s", options.baseURL, url.PathEscape(signerID))
	responsePayload := APIKeysPageResponse{}
	if err := sendHTTPRequestToCNIL(
		httpClient,
		http.MethodGet,
		url,
		options.token,
		http.StatusOK,
		nil,
		&responsePayload,
	); err != nil {
		return nil, err
	}

	if len(responsePayload.Items) == 0 {
		return nil, errAPIKeyNotFound
	}

	return responsePayload.Items[0], nil
}

type APIKeyCreateReq struct {
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only"`
}

func createAPIKey(
	httpClient *http.Client,
	options *cnilOptions,
	signerID string,
) (*APIKeyResponse, error) {

	url := fmt.Sprintf("%s/ledgers/%s/api_keys", options.baseURL, options.ledgerID)

	payload := APIKeyCreateReq{Name: signerID}
	payloadJSON, err := json.Marshal(&payload)
	if err != nil {
		return nil, fmt.Errorf(
			"error JSON-marshaling POST %s request with payload %+v: %v",
			url, payload, err)
	}

	responsePayload := APIKeyResponse{}
	if err := sendHTTPRequestToCNIL(
		httpClient,
		http.MethodPost,
		url,
		options.token,
		http.StatusCreated,
		bytes.NewBuffer(payloadJSON),
		&responsePayload,
	); err != nil {
		return nil, err
	}

	return &responsePayload, nil
}

func rotateAPIKey(
	httpClient *http.Client,
	options *cnilOptions,
	apiKeyID string,
) (*APIKeyResponse, error) {

	url := fmt.Sprintf("%s/ledgers/%s/api_keys/%s/rotate", options.baseURL, options.ledgerID, apiKeyID)
	responsePayload := APIKeyResponse{}
	if err := sendHTTPRequestToCNIL(
		httpClient,
		http.MethodPut,
		url,
		options.token,
		http.StatusOK,
		nil,
		&responsePayload,
	); err != nil {
		return nil, err
	}

	return &responsePayload, nil
}

func sendHTTPRequestToCNIL(
	httpClient *http.Client,
	method string,
	url string,
	token string,
	expectedStatus int,
	payload io.Reader,
	responsePayload interface{},
) error {
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		return fmt.Errorf("error creating HTTP request %s %s: %v", method, url, err)
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+token)

	response, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request %s %s: %v", method, url, err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("%s %s: error reading response body: %v", method, url, err)
	}

	if response.StatusCode != expectedStatus {
		return fmt.Errorf("%s %s error: expected response status %d, got %s with body %s",
			method, url, expectedStatus, response.Status, responseBody)
	}

	if err := json.Unmarshal(responseBody, responsePayload); err != nil {
		return fmt.Errorf("error JSON-unmarshaling %s %s response body %s: %v",
			method, url, responseBody, err)
	}

	return nil
}

type vcnOptions struct {
	storeDir   string
	cnilHost   string
	cnilPort   string
	cnilAPIKey string
}

func vcnArtifactFromAssetFile(filePath string) (*vcnAPI.Artifact, error) {
	fileURI, err := vcnURI.Parse("file://" + filePath)
	if err != nil {
		return nil, fmt.Errorf("error parsing URI from asset file path %s: %v", filePath, err)
	}

	artifacts, err := vcnFileExtractor.Artifact(fileURI)
	if err != nil {
		return nil, fmt.Errorf("error creating vcn artifact from asset file %s: %v", fileURI, err)
	}

	return artifacts[0], nil
}

func notarizeAndVerify(
	vcnUser *vcnAPI.LcUser,
	artifact *vcnAPI.Artifact,
	options *vcnOptions,
) (*vcnAPI.LcArtifact, error) {

	var state vcnMeta.Status
	if _, _, err := vcnUser.Sign(*artifact, vcnAPI.LcSignWithStatus(state)); err != nil {
		return nil, fmt.Errorf("error signing artifact: %v", err)
	}

	notarizedArtifact, err := verify(vcnUser, artifact, options)
	if err != nil {
		return nil, fmt.Errorf(
			"%s was notarized without errors, but there was an error when verifying it: %v",
			artifact.Name, err)
	}
	if notarizedArtifact == nil {
		return nil, fmt.Errorf(
			"%s was notarized without error, but there was an error when verifying it: artifact not found",
			artifact.Name)
	}

	return notarizedArtifact, nil
}

func verify(
	vcnCNILUser *vcnAPI.LcUser,
	vcnArtifact *vcnAPI.Artifact,
	options *vcnOptions,
) (*vcnAPI.LcArtifact, error) {

	cnilArtifact, verified, err := vcnCNILUser.LoadArtifact(vcnArtifact.Hash, "", "", 0)
	if err == vcnAPI.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger might be compromised: %v", err)
	}

	if !verified {
		return nil, errors.New(
			`ledger might be compromised: CNIL verification status is "false"`)
	}

	if cnilArtifact.Revoked != nil && !cnilArtifact.Revoked.IsZero() {
		cnilArtifact.Status = vcnMeta.StatusApikeyRevoked
	}

	return cnilArtifact, nil
}

func coloredStatus(status vcnMeta.Status) string {
	statusColor := green
	switch status {
	case vcnMeta.StatusUntrusted, vcnMeta.StatusUnknown, vcnMeta.StatusUnsupported:
		statusColor = red
	case vcnMeta.StatusApikeyRevoked:
		statusColor = yellow
	}
	return fmt.Sprintf(statusColor, status)
}
