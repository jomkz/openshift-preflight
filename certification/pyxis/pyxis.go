package pyxis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/redhat-openshift-ecosystem/openshift-preflight/certification/errors"
	log "github.com/sirupsen/logrus"
)

const (
	apiVersion = "v1"
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type pyxisClient struct {
	ApiToken  string
	ProjectId string
	Client    HTTPClient
	PyxisHost string
}

func (p *pyxisClient) getPyxisUrl(path string) string {
	return fmt.Sprintf("https://%s/%s/%s", p.PyxisHost, apiVersion, path)
}

func (p *pyxisClient) getPyxisGraphqlUrl() string {
	return fmt.Sprintf("https://%s/graphql/", p.PyxisHost)
}

func NewPyxisClient(pyxisHost string, apiToken string, projectId string, httpClient HTTPClient) *pyxisClient {
	return &pyxisClient{
		ApiToken:  apiToken,
		ProjectId: projectId,
		Client:    httpClient,
		PyxisHost: pyxisHost,
	}
}

func (p *pyxisClient) createImage(ctx context.Context, certImage *CertImage) (*CertImage, error) {
	b, err := json.Marshal(certImage)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	req, err := p.newRequestWithApiToken(ctx, http.MethodPost, p.getPyxisUrl("images"), bytes.NewReader(b))
	if err != nil {
		log.Error(err)
		return nil, err
	}

	log.Debugf("URL is: %s", req.URL)

	resp, err := p.Client.Do(req)
	if err != nil {
		log.Error(fmt.Errorf("%w: cannot create image", err))
		return nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	if resp.StatusCode == http.StatusConflict {
		return nil, errors.ErrPyxis409StatusCode
	}

	if !checkStatus(resp.StatusCode) {
		log.Errorf("%s: %s", "received non 200 status code in createImage", string(body))
		return nil, errors.ErrNon200StatusCode
	}

	var newCertImage CertImage
	if err := json.Unmarshal(body, &newCertImage); err != nil {
		log.Error(err)
		return nil, err
	}

	return &newCertImage, nil
}

func (p *pyxisClient) getImage(ctx context.Context, dockerImageDigest string) (*CertImage, error) {
	req, err := p.newRequestWithApiToken(ctx, http.MethodGet,
		p.getPyxisUrl(fmt.Sprintf("projects/certification/id/%s/images?filter=docker_image_digest==%s", p.ProjectId, dockerImageDigest)), nil)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	log.Debugf("URL is: %s", req.URL)

	resp, err := p.Client.Do(req)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	if !checkStatus(resp.StatusCode) {
		log.Errorf("%s: %s", "received non 200 status code in getImage", string(body))
		return nil, errors.ErrNon200StatusCode
	}

	// using an inline struct since this api's response is in a different format
	data := struct {
		Data []CertImage `json:"data"`
	}{}

	if err := json.Unmarshal(body, &data); err != nil {
		log.Error(err)
		return nil, err
	}

	return &data.Data[0], nil
}

func (p *pyxisClient) createRPMManifest(ctx context.Context, rpmManifest *RPMManifest) (*RPMManifest, error) {
	b, err := json.Marshal(rpmManifest)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	req, err := p.newRequestWithApiToken(ctx, http.MethodPost, p.getPyxisUrl(fmt.Sprintf("images/id/%s/rpm-manifest", rpmManifest.ImageID)), bytes.NewReader(b))
	if err != nil {
		log.Error(err)
		return nil, err
	}

	log.Debugf("URL is: %s", req.URL)

	resp, err := p.Client.Do(req)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	if resp.StatusCode == 409 {
		return nil, errors.ErrPyxis409StatusCode
	}

	if !checkStatus(resp.StatusCode) {
		log.Errorf("%s: %s", "received non 200 status code in createRPMManifest", string(body))
		return nil, errors.ErrNon200StatusCode
	}

	var newRPMManifest RPMManifest
	if err := json.Unmarshal(body, &newRPMManifest); err != nil {
		log.Error(err)
		return nil, err
	}

	return &newRPMManifest, nil
}

func (p *pyxisClient) getRPMManifest(ctx context.Context, imageID string) (*RPMManifest, error) {
	req, err := p.newRequestWithApiToken(ctx, http.MethodGet, p.getPyxisUrl(fmt.Sprintf("images/id/%s/rpm-manifest", imageID)), nil)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	log.Debugf("URL is: %s", req.URL)

	resp, err := p.Client.Do(req)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	if !checkStatus(resp.StatusCode) {
		log.Errorf("%s: %s", "received non 200 status code in getRPMManifest", string(body))
		return nil, errors.ErrNon200StatusCode
	}

	var newRPMManifest RPMManifest
	if err := json.Unmarshal(body, &newRPMManifest); err != nil {
		log.Error(err)
		return nil, err
	}

	return &newRPMManifest, nil
}

func (p *pyxisClient) GetProject(ctx context.Context) (*CertProject, error) {
	req, err := p.newRequestWithApiToken(ctx, http.MethodGet, p.getPyxisUrl(fmt.Sprintf("projects/certification/id/%s", p.ProjectId)), nil)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	log.Debugf("URL is: %s", req.URL)

	resp, err := p.Client.Do(req)
	if err != nil {
		log.Error(err, "client.Do failed")
		return nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error(err, "readall failed")
		return nil, err
	}

	if !checkStatus(resp.StatusCode) {
		log.Errorf("%s: %s", "received non 200 status code in GetProject", string(body))
		return nil, errors.ErrNon200StatusCode
	}

	var certProject CertProject
	if err := json.Unmarshal(body, &certProject); err != nil {
		log.Error(err)
		return nil, err
	}

	return &certProject, nil
}

func (p *pyxisClient) updateProject(ctx context.Context, certProject *CertProject) (*CertProject, error) {
	b, err := json.Marshal(certProject)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	req, err := p.newRequestWithApiToken(ctx, http.MethodPatch, p.getPyxisUrl(fmt.Sprintf("projects/certification/id/%s", p.ProjectId)), bytes.NewReader(b))
	if err != nil {
		log.Error(err)
		return nil, err
	}

	log.Debugf("URL is: %s", req.URL)

	resp, err := p.Client.Do(req)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	if !checkStatus(resp.StatusCode) {
		log.Errorf("%s: %s", "received non 200 status code in updateProject", string(body))
		return nil, errors.ErrNon200StatusCode
	}

	var newCertProject CertProject
	if err := json.Unmarshal(body, &newCertProject); err != nil {
		log.Error(err)
		return nil, err
	}

	return &newCertProject, nil
}

func (p *pyxisClient) createTestResults(ctx context.Context, testResults *TestResults) (*TestResults, error) {
	b, err := json.Marshal(testResults)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	req, err := p.newRequestWithApiToken(ctx, http.MethodPost, p.getPyxisUrl(fmt.Sprintf("projects/certification/id/%s/test-results", p.ProjectId)), bytes.NewReader(b))
	if err != nil {
		log.Error(err)
		return nil, err
	}

	resp, err := p.Client.Do(req)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	if !checkStatus(resp.StatusCode) {
		log.Errorf("%s: %s", "received non 200 status code in createTestResults", string(body))
		return nil, errors.ErrNon200StatusCode
	}

	newTestResults := TestResults{}
	if err := json.Unmarshal(body, &newTestResults); err != nil {
		log.Error(err)
		return nil, err
	}

	return &newTestResults, nil
}

func (p *pyxisClient) createArtifact(ctx context.Context, artifact *Artifact) (*Artifact, error) {
	b, err := json.Marshal(artifact)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	req, err := p.newRequestWithApiToken(ctx, http.MethodPost, p.getPyxisUrl(fmt.Sprintf("projects/certification/id/%s/artifacts", p.ProjectId)), bytes.NewReader(b))
	if err != nil {
		log.Error(err)
		return nil, err
	}

	log.Debugf("URL is: %s", req.URL)

	resp, err := p.Client.Do(req)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	if !checkStatus(resp.StatusCode) {
		log.Errorf("%s: %s", "received non 200 status code in createArtifact", string(body))
		return nil, errors.ErrNon200StatusCode
	}

	var newArtifact Artifact
	if err := json.Unmarshal(body, &newArtifact); err != nil {
		log.Error(err)
		return nil, err
	}

	return &newArtifact, nil
}

func (p *pyxisClient) newRequestWithApiToken(ctx context.Context, method string, url string, body io.Reader) (*http.Request, error) {
	req, err := p.newRequest(ctx, method, url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Add("X-API-KEY", p.ApiToken)

	return req, nil
}

func (p *pyxisClient) newRequest(ctx context.Context, method string, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Add("Content-type", "application/json")
	}

	return req, nil
}

// checkStatus is used to check for a 2xx status code
func checkStatus(statusCode int) bool {
	return statusCode >= 200 && statusCode < 300
}
