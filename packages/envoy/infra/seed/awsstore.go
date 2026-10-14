package seed

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
)

// AWSSecretsManager is the --store value of AWS Secrets Manager.
const AWSSecretsManager = "aws-secrets-manager"

// awsCurrent is the staging label of the version Secrets Manager serves by default.
const awsCurrent = "AWSCURRENT"

var awsAccountID = regexp.MustCompile(`^[0-9]{12}$`)

// OpenAWS opens Secrets Manager under the caller's own AWS configuration (the environment, then the
// shared files), in region when it is not "". With account, it first refuses unless
// sts:GetCallerIdentity answers that account, so a seed never lands in, or is read from, another
// account's secret of the same name.
func OpenAWS(ctx context.Context, region, account string) (Store, error) {
	if account != "" && !awsAccountID.MatchString(account) {
		return nil, refuse("--expect-account is a 12-digit AWS account id")
	}
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return nil, refuse("load the AWS configuration: %v", err)
	}
	if cfg.Region == "" {
		return nil, refuse("no AWS region: pass --region, or name one in AWS_REGION or the profile")
	}
	if account != "" {
		identity, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			return nil, refuse("sts get-caller-identity failed: %s", errorClass(err))
		}
		if got := aws.ToString(identity.Account); got != account {
			return nil, refuse("these credentials are for account %s, but --expect-account is %s; use that account's credentials", got, account)
		}
	}
	return &awsStore{client: secretsmanager.NewFromConfig(cfg)}, nil
}

// loadAWSConfig loads the caller's AWS configuration with the SDK's logging off: its logger and its
// client log modes can print request and response bodies, and a write carries the seed in one, a
// read in the other.
func loadAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	options := []func(*config.LoadOptions) error{
		config.WithLogger(logging.Nop{}),
		config.WithClientLogMode(0),
	}
	if region != "" {
		options = append(options, config.WithRegion(region))
	}
	return config.LoadDefaultConfig(ctx, options...)
}

// awsStore is Store over Secrets Manager. Every error names the call, the secret and the AWS error
// code (or the SDK error's class), never the error's own message, which can quote the request.
type awsStore struct{ client *secretsmanager.Client }

// Label is AWSCURRENT.
func (s *awsStore) Label() string { return awsCurrent }

// Describe reads DescribeSecret alone.
func (s *awsStore) Describe(ctx context.Context, name string) (Description, error) {
	out, err := s.client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	var notFound *types.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return Description{}, nil
	}
	if err != nil {
		return Description{}, awsFailed("describe-secret", name, err)
	}
	if out.DeletedDate != nil {
		return Description{Exists: true, ScheduledForDeletion: true, DeletionDate: pythonDate(*out.DeletedDate, time.Local)}, nil
	}
	d := Description{Exists: true}
	// AWS caps a secret's staging labels at 20 across its versions, so this map, unpaginated, is
	// every label.
	for version, stages := range out.VersionIdsToStages {
		if slices.Contains(stages, awsCurrent) {
			d.CurrentVersion = version
		}
	}
	return d, nil
}

// Versions counts every version ListSecretVersionIds lists with IncludeDeprecated, across every
// page: an unlabelled version is still a seed a client may have read.
func (s *awsStore) Versions(ctx context.Context, name string) (int, error) {
	in := &secretsmanager.ListSecretVersionIdsInput{SecretId: aws.String(name), IncludeDeprecated: aws.Bool(true)}
	handedOut := map[string]bool{}
	count := 0
	for {
		page, err := s.client.ListSecretVersionIds(ctx, in)
		if err != nil {
			return 0, awsFailed("list-secret-version-ids", name, err)
		}
		for _, v := range page.Versions {
			if v.VersionId != nil {
				count++
			}
		}
		token := aws.ToString(page.NextToken)
		if token == "" {
			return count, nil
		}
		// The SDK's paginator stops quietly on a repeated token, which would undercount versions;
		// following it would never end.
		if handedOut[token] {
			return 0, fmt.Errorf("list-secret-version-ids %s failed: the listing handed out page token %s twice", name, token)
		}
		handedOut[token] = true
		in.NextToken = aws.String(token)
	}
}

// Put writes value as a SecretString. The SDK fills in a fresh ClientRequestToken for each call and
// its retries resend that token, so a lost answer retried writes no second version. The written
// version must be AWSCURRENT.
func (s *awsStore) Put(ctx context.Context, name string, value []byte) (string, error) {
	out, err := s.client.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId:     aws.String(name),
		SecretString: aws.String(string(value)),
	})
	if err != nil {
		return "", awsFailed("put-secret-value", name, err)
	}
	version := aws.ToString(out.VersionId)
	if version == "" {
		return "", fmt.Errorf("put-secret-value %s answered no version id", name)
	}
	if !slices.Contains(out.VersionStages, awsCurrent) {
		return version, fmt.Errorf("version %s was written but is not %s; run check", version, awsCurrent)
	}
	return version, nil
}

// Current reads the AWSCURRENT version's SecretString.
func (s *awsStore) Current(ctx context.Context, name string) (string, []byte, error) {
	out, err := s.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(name)})
	if err != nil {
		return "", nil, awsFailed("get-secret-value", name, err)
	}
	return aws.ToString(out.VersionId), []byte(aws.ToString(out.SecretString)), nil
}

// awsFailed is "<call> <secret> failed: <code>": an AWS error's code, or the SDK error's class.
func awsFailed(call, name string, err error) error {
	return fmt.Errorf("%s %s failed: %s", call, name, errorClass(err))
}

// sdkWrappers are the error types the SDK and fmt wrap a failure in, which name nothing of it: a
// lost answer is an OperationError around a MaxAttemptsError around a ResponseError (both the AWS
// and the smithy one are http.ResponseError) around the RequestSendError that says what happened.
var sdkWrappers = []string{"*smithy.OperationError", "*retry.MaxAttemptsError", "*http.ResponseError", "*fmt.wrapError"}

// errorClass names err without its text: an AWS error's code; for a context's end, that; otherwise
// the type of the first error in its chain that is not one of the SDK's wrappers, such as
// RequestSendError.
func errorClass(err error) string {
	var api smithy.APIError
	switch {
	case errors.As(err, &api) && api.ErrorCode() != "":
		return api.ErrorCode()
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	}
	for e := err; ; {
		name := reflect.TypeOf(e).String()
		next := errors.Unwrap(e)
		if next == nil || !slices.Contains(sdkWrappers, name) {
			return name[strings.LastIndex(name, ".")+1:]
		}
		e = next
	}
}

// pythonDate is t in loc as Python's str prints a datetime: microseconds only when there are some,
// then the offset as ±hh:mm. check prints a deletion date in this form.
func pythonDate(t time.Time, loc *time.Location) string {
	t = t.In(loc)
	text := t.Format("2006-01-02 15:04:05")
	if micro := t.Nanosecond() / 1000; micro != 0 {
		text += fmt.Sprintf(".%06d", micro)
	}
	return text + t.Format("-07:00")
}
