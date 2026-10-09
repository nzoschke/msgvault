package daemonclient

import (
	"context"
	"errors"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) CreateImportJob(ctx context.Context, in generated.ImportJobRequest) (*generated.ImportJobResponse, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.CreateImportJobResp, error) {
		return client.CreateImportJobWithResponse(ctx, &generated.CreateImportJobRequestOptions{Body: &in})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON202 == nil {
		return nil, errors.New("import job returned an empty response")
	}
	return response.JSON202, nil
}
func (c *Client) GetImportJob(ctx context.Context, id string) (*generated.ImportJobResponse, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetImportJobResp, error) {
		return client.GetImportJobWithResponse(ctx, &generated.GetImportJobRequestOptions{PathParams: &generated.GetImportJobPath{JobID: id}})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("import job returned an empty response")
	}
	return response.JSON200, nil
}
