package daemonclient

import (
	"context"
	"errors"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) ImportMessages(ctx context.Context, in generated.ImportMessagesRequest) (*generated.ImportMessagesResponse, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.ImportMessagesResp, error) {
		return client.ImportMessagesWithResponse(ctx, &generated.ImportMessagesRequestOptions{Body: &in})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("message import returned an empty response")
	}
	return response.JSON200, nil
}
