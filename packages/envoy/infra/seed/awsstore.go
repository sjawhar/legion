package seed

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
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
	smithyendpoints "github.com/aws/smithy-go/endpoints"
	"github.com/aws/smithy-go/logging"

	"github.com/sjawhar/envoy/internal/bus"
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
// shared files), in region when it is not "". Before any client is built or any credential
// retrieved, it refuses a seed anywhere that configuration carries one (checkedAWSConfig); then a
// seed in the endpoints the command sends to, as the two clients resolve them (endpointSeed), and
// a configured Secrets Manager endpoint a seed would cross a network to in plain text
// (endpointCheck); then credentials it cannot retrieve, naming the fix. Building a client sends
// nothing: the first request is the credentials' own. With account, it then refuses unless
// sts:GetCallerIdentity answers that account; Describe holds the secret itself to that account
// too, so a seed never lands in, or is read from, another account's secret of the same name.
func OpenAWS(ctx context.Context, region, account string) (Store, error) {
	if account != "" && !awsAccountID.MatchString(account) {
		return nil, refuse("--expect-account is a 12-digit AWS account id")
	}
	cfg, err := checkedAWSConfig(ctx, region)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		return nil, refuse("no AWS region: pass --region, or name one in AWS_REGION or the profile")
	}
	client := secretsmanager.NewFromConfig(cfg)
	identity := sts.NewFromConfig(cfg)
	if source := endpointSeed(ctx, client.Options(), identity.Options()); source != "" {
		return nil, awsConfigSeed(source)
	}
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
		answer, err := identity.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			return nil, refuse("sts get-caller-identity failed: %s", errorClass(err))
		}
		if got := aws.ToString(answer.Account); got != account {
			return nil, refuse("these credentials are for account %s, but --expect-account is %s; use that account's credentials", got, account)
		}
	}
	return &awsStore{client: client, region: cfg.Region, account: account}, nil
}

// checkedAWSConfig loads the caller's AWS configuration and refuses a seed (bus.HoldsSeed)
// anywhere in what it carries into a client, naming where it came from and never the value
// (awsConfigSeed). --region, the environment the SDK reads and the files' default paths are checked
// before loading, since the loader's own errors quote values: a profile it cannot find, an
// AWS_MAX_ATTEMPTS that is no number. A load error that still quotes a seed quotes the files'.
// After loading, configSeed checks every profile the loader read and the resolved aws.Config.
func checkedAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	if bus.HoldsSeed(region) {
		return aws.Config{}, awsConfigSeed("--region")
	}
	if name := awsEnvironmentSeed(); name != "" {
		return aws.Config{}, awsConfigSeed(name)
	}
	for _, file := range []struct{ source, path string }{
		{"the shared config file's default path", config.DefaultSharedConfigFilename()},
		{"the shared credentials file's default path", config.DefaultSharedCredentialsFilename()},
	} {
		if bus.HoldsSeed(file.path) {
			return aws.Config{}, awsConfigSeed(file.source)
		}
	}
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		if source := failedLoadSeed(ctx); source != "" {
			return aws.Config{}, awsConfigSeed(source)
		}
		var missing config.SharedConfigProfileNotExistError
		switch {
		case errors.As(err, &missing) && bus.HoldsSeed(missing.Profile):
			return aws.Config{}, awsConfigSeed("a profile the shared config names")
		case bus.HoldsSeed(err.Error()):
			return aws.Config{}, awsConfigSeed("a value the shared config or credentials file holds")
		}
		return aws.Config{}, refuse("load the AWS configuration: %v", err)
	}
	if source := configSeed(cfg); source != "" {
		return aws.Config{}, awsConfigSeed(source)
	}
	return cfg, nil
}

// awsEnvironmentSeed names the first variable, in name order, of the environment the SDK reads that
// holds a seed, or returns "". The SDK reads only AWS_ variables, the per-service endpoint
// variables AWS_ENDPOINT_URL_<SERVICE> among them, and _X_AMZN_TRACE_ID, which it sends as the
// X-Amzn-Trace-Id header on Lambda (aws-sdk-go-v2 v1.47.3 aws/middleware/recursion_detection.go),
// so every such variable is checked, whether this SDK version reads it or not.
func awsEnvironmentSeed() string {
	var names []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "AWS_") || name == "_X_AMZN_TRACE_ID" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		if bus.HoldsSeed(os.Getenv(name)) {
			return name
		}
	}
	return ""
}

// configSeed names where the loaded configuration holds a seed, or returns "": each shared config
// profile the loader read (sharedConfigSeed), then every string of the resolved aws.Config. The
// environment was checked before loading.
func configSeed(cfg aws.Config) string {
	for _, source := range cfg.ConfigSources {
		if shared, ok := source.(config.SharedConfig); ok {
			if where := sharedConfigSeed(shared); where != "" {
				return where
			}
		}
	}
	if path, ok := seedPath(reflect.ValueOf(cfg)); ok {
		return "the resolved AWS configuration's " + path
	}
	return ""
}

// failedLoadSeed reads, for a load that failed, the shared config profile the loader reads, from
// the files it reads, on its own, and names where that profile holds a seed, or returns "". A
// failure that comes after the files are read, such as an mfa_serial with no token provider, then
// still names the seed rather than the failure.
func failedLoadSeed(ctx context.Context) string {
	env, err := config.NewEnvConfig()
	if err != nil {
		return ""
	}
	profile := env.SharedConfigProfile
	if profile == "" {
		profile = config.DefaultSharedConfigProfile
	}
	shared, err := config.LoadSharedConfigProfile(ctx, profile, func(o *config.LoadSharedConfigOptions) {
		if env.SharedConfigFile != "" {
			o.ConfigFiles = []string{env.SharedConfigFile}
		}
		if env.SharedCredentialsFile != "" {
			o.CredentialsFiles = []string{env.SharedCredentialsFile}
		}
		o.Logger = logging.Nop{}
	})
	if err != nil {
		return ""
	}
	return sharedConfigSeed(shared)
}

// sharedConfigSeed names where a shared config profile, or the source profile it names, as a
// profile of its own, holds a seed (profileSeed), or returns "".
func sharedConfigSeed(shared config.SharedConfig) string {
	for level := &shared; level != nil; level = level.Source {
		if where := profileSeed(*level); where != "" {
			return where
		}
	}
	return ""
}

// profileSeed names where one shared config profile holds a seed, by its ini key, or returns "".
// Its source profile is left to the caller, as a level of its own named by its own profile name,
// which is the source_profile this level has already checked.
func profileSeed(c config.SharedConfig) string {
	if bus.HoldsSeed(c.Profile) {
		return "the name of a profile the shared config reads"
	}
	c.Source = nil
	path, ok := seedPath(reflect.ValueOf(c))
	if !ok {
		return ""
	}
	return fmt.Sprintf("the shared config's profile %q: %s", c.Profile, sharedConfigKey(path))
}

// sharedConfigKeys are the ini keys of config.SharedConfig's string fields, by seedPath's path
// (aws-sdk-go-v2/config v1.33.9 shared_config.go:40-130). A field the map lacks is named by its path.
var sharedConfigKeys = map[string]string{
	"Credentials.AccessKeyID":     "aws_access_key_id",
	"Credentials.SecretAccessKey": "aws_secret_access_key",
	"Credentials.SessionToken":    "aws_session_token",
	"Credentials.Source":          "the path of its credentials file",
	"Credentials.AccountID":       "aws_account_id",
	"CredentialSource":            "credential_source",
	"CredentialProcess":           "credential_process",
	"WebIdentityTokenFile":        "web_identity_token_file",
	"SSOSessionName":              "sso_session",
	"SSOSession.Name":             "sso_session",
	"SSOSession.SSORegion":        "the sso_region of its sso-session",
	"SSOSession.SSOStartURL":      "the sso_start_url of its sso-session",
	"SSORegion":                   "sso_region",
	"SSOStartURL":                 "sso_start_url",
	"SSOAccountID":                "sso_account_id",
	"SSORoleName":                 "sso_role_name",
	"RoleARN":                     "role_arn",
	"ExternalID":                  "external_id",
	"MFASerial":                   "mfa_serial",
	"RoleSessionName":             "role_session_name",
	"SourceProfileName":           "source_profile",
	"Region":                      "region",
	"EC2IMDSEndpoint":             "ec2_metadata_service_endpoint",
	"CustomCABundle":              "ca_bundle",
	"AppID":                       "sdk_ua_app_id",
	"BaseEndpoint":                "endpoint_url",
	"ServicesSectionName":         "services",
	"Services.ServiceValues":      "a service its services section names",
	"AuthSchemePreference":        "auth_scheme_preference",
	"LoginSession":                "login_session",
}

// sharedConfigKey names path, a seedPath of config.SharedConfig, by its ini key.
func sharedConfigKey(path string) string {
	if key, ok := sharedConfigKeys[path]; ok {
		return key
	}
	if rest, ok := strings.CutPrefix(path, "Services.ServiceValues["); ok {
		service, key, _ := strings.Cut(strings.TrimSuffix(rest, "]"), "][")
		if key == "" {
			return "a key of the " + service + " section of its services section"
		}
		return "the " + service + " " + key + " of its services section"
	}
	return path
}

// seedPath returns where in v a string holds a seed (bus.HoldsSeed): exported field names joined
// by ".", and map keys in brackets. It follows pointers, slices and maps, never interfaces or
// functions, which hold code rather than configuration. A map key that holds a seed ends the path
// at its map, so the path never quotes it.
func seedPath(v reflect.Value) (string, bool) {
	switch v.Kind() {
	case reflect.String:
		return "", bus.HoldsSeed(v.String())
	case reflect.Pointer:
		if v.IsNil() {
			return "", false
		}
		return seedPath(v.Elem())
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if path, ok := seedPath(v.Index(i)); ok {
				return path, true
			}
		}
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
		for _, key := range keys {
			if _, ok := seedPath(key); ok {
				return "", true
			}
			if path, ok := seedPath(v.MapIndex(key)); ok {
				return fieldPath("["+fmt.Sprint(key)+"]", path), true
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			if path, ok := seedPath(v.Field(i)); ok {
				return fieldPath(field.Name, path), true
			}
		}
	}
	return "", false
}

// fieldPath joins name and the path inside it.
func fieldPath(name, rest string) string {
	switch {
	case rest == "":
		return name
	case strings.HasPrefix(rest, "["):
		return name + rest
	}
	return name + "." + rest
}

// endpointSeed resolves the endpoints the command sends to with the clients' own resolvers, the
// parameters bound as each client binds them (bindEndpointParams in secretsmanager v1.50.4 and sts
// v1.51.4 endpoints.go): Secrets Manager's, and STS's, regional and global. It names one whose URL
// or headers hold a seed, or returns "". bus.HoldsSeed reads every part of a URL, path, query,
// fragment and userinfo, in either case, percent-encoded or not. The SDK resolves an endpoint only
// as it sends a request, so this resolves them before the first one; an endpoint that fails to
// resolve is one no request is sent to either.
func endpointSeed(ctx context.Context, secrets secretsmanager.Options, identity sts.Options) string {
	holds := func(endpoint smithyendpoints.Endpoint, err error) bool {
		if err != nil {
			return false
		}
		if bus.HoldsSeed(endpoint.URI.String()) {
			return true
		}
		for _, values := range endpoint.Headers {
			if slices.ContainsFunc(values, bus.HoldsSeed) {
				return true
			}
		}
		return false
	}
	if holds(secrets.EndpointResolverV2.ResolveEndpoint(ctx, secretsmanager.EndpointParameters{
		Region:       aws.String(secrets.Region),
		UseDualStack: aws.Bool(secrets.EndpointOptions.UseDualStackEndpoint == aws.DualStackEndpointStateEnabled),
		UseFIPS:      aws.Bool(secrets.EndpointOptions.UseFIPSEndpoint == aws.FIPSEndpointStateEnabled),
		Endpoint:     secrets.BaseEndpoint,
	})) {
		return "the Secrets Manager endpoint"
	}
	for _, global := range []bool{false, true} {
		if holds(identity.EndpointResolverV2.ResolveEndpoint(ctx, sts.EndpointParameters{
			Region:            aws.String(identity.Region),
			UseDualStack:      aws.Bool(identity.EndpointOptions.UseDualStackEndpoint == aws.DualStackEndpointStateEnabled),
			UseFIPS:           aws.Bool(identity.EndpointOptions.UseFIPSEndpoint == aws.FIPSEndpointStateEnabled),
			Endpoint:          identity.BaseEndpoint,
			UseGlobalEndpoint: aws.Bool(global),
		})) {
			if global {
				return "the STS global endpoint"
			}
			return "the STS endpoint"
		}
	}
	return ""
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

// awsConfigSeed refuses a value of the AWS configuration that holds an nkey seed, naming source,
// where it came from.
func awsConfigSeed(source string) error {
	return refuse("%s: an nkey seed, which is never AWS configuration (not echoed)", source)
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
