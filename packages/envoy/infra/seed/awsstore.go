package seed

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
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

// firstVersionToken is the ClientRequestToken of every write generate makes, which is only ever a
// secret's first version. Secrets Manager names the version by the token: for a token it already
// holds, it writes nothing when the value is the same and refuses with ResourceExistsException when
// it differs. So an SDK retry of a lost answer writes no second version, and a second run's
// first-version write with another seed is refused rather than written beside the first. A version
// id is per secret, so one constant serves every secret.
const firstVersionToken = "037f74cb-0e19-4a2d-b95f-6fba44b44b4d"

var awsAccountID = regexp.MustCompile(`^[0-9]{12}$`)

// OpenAWS opens Secrets Manager under the caller's own AWS configuration (the environment, then the
// shared files), in region when it is not "". Before any call it refuses a configured Secrets
// Manager endpoint a seed would cross a network to in plain text (endpointCheck), and credentials
// it cannot retrieve, naming the fix. With account, it then refuses unless sts:GetCallerIdentity
// answers that account; Describe holds the secret itself to that account too, so a seed never
// lands in, or is read from, another account's secret of the same name.
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
	client := secretsmanager.NewFromConfig(cfg)
	if err := endpointCheck(client.Options().BaseEndpoint); err != nil {
		return nil, err
	}
	// The credentials cache keeps what this retrieves, so the calls below sign with it. A chain that
	// finds no credentials fails with a plain error whose type names nothing, so only a named
	// failure, such as an AWS error's code, is given.
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		if name, ok := errorName(err); ok {
			return nil, refuse("no usable AWS credentials (%s); log in or set AWS_PROFILE", name)
		}
		return nil, refuse("no usable AWS credentials; log in or set AWS_PROFILE")
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
	return &awsStore{client: client, region: cfg.Region, account: account}, nil
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

// endpointCheck refuses a configured Secrets Manager endpoint (AWS_ENDPOINT_URL,
// AWS_ENDPOINT_URL_SECRETS_MANAGER or a profile's services section; nil when none is, and the SDK's
// own endpoint for the region, which is https, is used) that is not https, unless it is this
// machine's own loopback address, as a local test endpoint is: a seed travels in a PutSecretValue
// request and a GetSecretValue answer, and plain text sent to loopback crosses no network.
func endpointCheck(endpoint *string) error {
	if endpoint == nil {
		return nil
	}
	u, err := url.Parse(*endpoint)
	if err != nil {
		return refuse("the Secrets Manager endpoint is not a URL")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme == "https" || u.Scheme == "http" && (host == "localhost" || ip != nil && ip.IsLoopback()) {
		return nil
	}
	return refuse("the Secrets Manager endpoint %s is not https, and is not this machine's loopback address: a seed is sent in plain text only to this machine", u.Redacted())
}

// awsStore is Store over Secrets Manager. Every error names the call, the secret and the AWS error
// code (or the SDK error's class), never the error's own message, which can quote the request.
type awsStore struct {
	client  *secretsmanager.Client
	region  string // the region the client sends to
	account string // --expect-account; "" when it was not given
}

// Describe reads DescribeSecret alone, and refuses a secret whose ARN puts it in another region than
// the client's, or, with --expect-account, in another account: STS and Secrets Manager each resolve
// their own endpoint, so the account STS answered need not be the account of this secret.
func (s *awsStore) Describe(ctx context.Context, name string) (Description, error) {
	out, err := s.client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	var notFound *types.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return Description{}, nil
	}
	if err != nil {
		return Description{}, awsFailed("describe-secret", name, err)
	}
	if err := s.expected(name, aws.ToString(out.ARN)); err != nil {
		return Description{}, err
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

// expected refuses the secret whose ARN is secretARN unless it is a Secrets Manager secret in the
// client's region and, with --expect-account, in that account.
func (s *awsStore) expected(name, secretARN string) error {
	parsed, err := arn.Parse(secretARN)
	switch {
	case err != nil || parsed.Service != "secretsmanager":
		return fmt.Errorf("describe-secret %s answered no Secrets Manager ARN, so the secret's account and region cannot be checked", name)
	case s.account != "" && parsed.AccountID != s.account:
		return fmt.Errorf("describe-secret %s answered a secret of account %s, but --expect-account is %s: the Secrets Manager endpoint answers for another account",
			name, parsed.AccountID, s.account)
	case parsed.Region != s.region:
		return fmt.Errorf("describe-secret %s answered a secret in region %s, but the client is in region %s", name, parsed.Region, s.region)
	}
	return nil
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

// Put writes value as a SecretString under firstVersionToken, which the SDK's retries resend. A
// first version already written with another value is ResourceExistsException, a refusal: nothing
// was written. The written version must be AWSCURRENT.
func (s *awsStore) Put(ctx context.Context, name string, value []byte) (string, error) {
	out, err := s.client.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId:           aws.String(name),
		SecretString:       aws.String(string(value)),
		ClientRequestToken: aws.String(firstVersionToken),
	})
	var exists *types.ResourceExistsException
	if errors.As(err, &exists) {
		return "", refuse("%v; another generate wrote this secret's first version, with another seed", awsFailed("put-secret-value", name, err))
	}
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

// Current reads the AWSCURRENT version's SecretString. Secrets Manager answers
// ResourceNotFoundException when no version is AWSCURRENT, as just after a first write it may.
func (s *awsStore) Current(ctx context.Context, name string) (string, []byte, error) {
	out, err := s.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(name)})
	var notFound *types.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return "", nil, notServed{awsFailed("get-secret-value", name, err)}
	}
	if err != nil {
		return "", nil, awsFailed("get-secret-value", name, err)
	}
	return aws.ToString(out.VersionId), []byte(aws.ToString(out.SecretString)), nil
}

// notServed is an error that reads as the store's own and matches ErrNotServed.
type notServed struct{ error }

func (notServed) Is(target error) bool { return target == ErrNotServed }

// awsFailed is "<call> <secret> failed: <code>": an AWS error's code, or the SDK error's class.
func awsFailed(call, name string, err error) error {
	return fmt.Errorf("%s %s failed: %s", call, name, errorClass(err))
}

// sdkWrappers are the error types the SDK and fmt wrap a failure in, which name nothing of it: a
// lost answer is an OperationError around a MaxAttemptsError around a ResponseError (both the AWS
// and the smithy one are http.ResponseError) around the RequestSendError that says what happened.
var sdkWrappers = []string{"*smithy.OperationError", "*retry.MaxAttemptsError", "*http.ResponseError", "*fmt.wrapError"}

// errorClass names err without its text: its name when it has one (errorName), otherwise the type
// of the first error in its chain that is not one of the SDK's wrappers, such as RequestSendError.
func errorClass(err error) string {
	if name, ok := errorName(err); ok {
		return name
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

// errorName names err when it says what happened without its text: an AWS error's code, or a
// context's end.
func errorName(err error) (string, bool) {
	var api smithy.APIError
	switch {
	case errors.As(err, &api) && api.ErrorCode() != "":
		return api.ErrorCode(), true
	case errors.Is(err, context.Canceled):
		return "canceled", true
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded", true
	}
	return "", false
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
