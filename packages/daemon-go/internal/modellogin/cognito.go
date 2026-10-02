package modellogin

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

// CognitoConfig is the machine login document's public Cognito connection information. Endpoint
// exists only for a local test server; production leaves it empty and the AWS SDK derives the
// Cognito endpoint from Region.
type CognitoConfig struct {
	Endpoint string
	Region   string
	ClientID string
}

type initiateAuthClient interface {
	InitiateAuth(context.Context, *cognitoidentityprovider.InitiateAuthInput, ...func(*cognitoidentityprovider.Options)) (*cognitoidentityprovider.InitiateAuthOutput, error)
}

type cognitoClient struct {
	client   initiateAuthClient
	clientID string
}

func NewCognitoClient(config CognitoConfig) Client {
	awsConfig := aws.Config{Region: config.Region}
	if config.Endpoint != "" {
		awsConfig.BaseEndpoint = aws.String(config.Endpoint)
	}
	return &cognitoClient{
		client:   cognitoidentityprovider.NewFromConfig(awsConfig),
		clientID: config.ClientID,
	}
}

func (c *cognitoClient) PasswordAuth(ctx context.Context, username, password string) (Result, error) {
	response, err := c.client.InitiateAuth(ctx, &cognitoidentityprovider.InitiateAuthInput{
		AuthFlow: types.AuthFlowTypeUserPasswordAuth,
		ClientId: aws.String(c.clientID),
		AuthParameters: map[string]string{
			"USERNAME": username,
			"PASSWORD": password,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("Cognito USER_PASSWORD_AUTH: %w", err)
	}
	return resultFrom(response)
}

func (c *cognitoClient) RefreshAuth(ctx context.Context, refreshToken string) (Result, error) {
	response, err := c.client.InitiateAuth(ctx, &cognitoidentityprovider.InitiateAuthInput{
		AuthFlow: types.AuthFlowTypeRefreshTokenAuth,
		ClientId: aws.String(c.clientID),
		AuthParameters: map[string]string{
			"REFRESH_TOKEN": refreshToken,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("Cognito REFRESH_TOKEN_AUTH: %w", err)
	}
	return resultFrom(response)
}

func resultFrom(response *cognitoidentityprovider.InitiateAuthOutput) (Result, error) {
	if response.AuthenticationResult == nil {
		return Result{}, fmt.Errorf("Cognito authentication returned no authentication result")
	}
	result := response.AuthenticationResult
	return Result{
		AccessToken:  aws.ToString(result.AccessToken),
		RefreshToken: aws.ToString(result.RefreshToken),
		ExpiresIn:    time.Duration(result.ExpiresIn) * time.Second,
	}, nil
}
