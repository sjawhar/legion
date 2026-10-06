// packages/envoy/cmd/agent-secrets/secret.go
//
// The secret forms: a person reads and writes agent secrets in AWS Secrets Manager under their own
// AWS sign-in, then asks the broker to reread each one written, so the change is served at once.
// The broker is asked only for its settings (GET /v1/settings: the namespace, key, account and
// region) and for the reread; every read and write is the person's own call to AWS, which IAM
// decides.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// secretForms are the secret forms, as cmdSecret dispatches them and its -h lists them.
var secretForms = []string{"list", "show", "create", "set", "retag", "delete", "restore"}

// recoveryWindowDays is the recovery window delete schedules every deletion with, 30 days: the
// longest Secrets Manager allows, during which restore brings the secret back.
const recoveryWindowDays = 30

// sharedRetagRefused is what a refused retag of a shared secret adds to AWS's own refusal: IAM
// lets only an administrator change a shared secret's owner or tier.
const sharedRetagRefused = "a shared secret's owner and tier are an administrator's to change"

// cmdSecret dispatches the secret forms.
func cmdSecret(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "agent-secrets secret: a form is required: %s\n", strings.Join(secretForms, ", "))
		return exitUsageError
	}
	switch args[0] {
	case "-h", "-help", "--help":
		for i, form := range secretForms {
			if i > 0 {
				fmt.Fprintln(stderr)
			}
			writeCommandHelp(stderr, lookupCommand("secret "+form))
		}
		return 0
	case "list":
		return cmdSecretList(args[1:], stdout, stderr)
	case "show":
		return cmdSecretShow(args[1:], stdout, stderr)
	case "create":
		return cmdSecretCreate(args[1:], stdout, stderr)
	case "set":
		return cmdSecretSet(args[1:], stdout, stderr)
	case "retag":
		return cmdSecretRetag(args[1:], stdout, stderr)
	case "delete":
		return cmdSecretDelete(args[1:], stdout, stderr)
	case "restore":
		return cmdSecretRestore(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "agent-secrets secret: unknown form %q; the forms are %s\n", args[0], strings.Join(secretForms, ", "))
		return exitUsageError
	}
}

// usageErr is a secret form's usage error, which exits 2.
type usageErr struct{ error }

// secretFail prints a secret form's failure and answers its exit code: 2 for a usage error, 1 for
// anything else.
func secretFail(stderr io.Writer, form string, err error) int {
	fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
	var usage usageErr
	if errors.As(err, &usage) {
		return exitUsageError
	}
	return 1
}

// secretFlagValues are the secret forms' flags that take a value.
var secretFlagValues = map[string]bool{"owner": true, "tier": true, "profile": true}

// secretFlags is form's flag set with --profile, which every secret form takes since every one
// calls AWS.
func secretFlags(form string, stderr io.Writer) (*flag.FlagSet, *string) {
	flags := newFlagSet(form, stderr)
	return flags, flags.String("profile", "", "the AWS profile to sign in with (default: AWS_PROFILE, else the AWS SDK's default credential chain)")
}

// secretName is the one NAME a form takes, and its slug: the secret's name under the prefix.
func secretName(positional []string) (name, slug string, err error) {
	if len(positional) != 1 {
		return "", "", usageErr{errors.New("exactly one secret NAME is required")}
	}
	slug, err = policy.NameToSlug(positional[0])
	if err != nil {
		return "", "", usageErr{fmt.Errorf("%q: %w", positional[0], err)}
	}
	return positional[0], slug, nil
}

// checkOwnerTier refuses an --owner that is neither me nor shared and a --tier that is neither
// agent nor human; "" passes either, for a form where it is optional.
func checkOwnerTier(owner, tier string) error {
	var errs []error
	if owner != "" && owner != "me" && owner != policy.OwnerShared {
		errs = append(errs, fmt.Errorf("--owner is me or %s, not %q", policy.OwnerShared, owner))
	}
	if tier != "" && tier != policy.TierAgent && tier != policy.TierHuman {
		errs = append(errs, fmt.Errorf("--tier is %s or %s, not %q", policy.TierAgent, policy.TierHuman, tier))
	}
	if len(errs) > 0 {
		return usageErr{errors.Join(errs...)}
	}
	return nil
}

// ownerTag is the owner tag --owner names: the signed-in person's email for me, shared for shared.
func ownerTag(owner, email string) string {
	if owner == "me" {
		return email
	}
	return owner
}

// readSecretValue reads a secret's value from secretStdin: all of it, less one trailing newline,
// so `echo` and a file ending in a newline give the value without one. An empty value is a usage
// error.
func readSecretValue() (string, error) {
	data, err := io.ReadAll(secretStdin)
	if err != nil {
		return "", fmt.Errorf("read the value from standard input: %w", err)
	}
	value := strings.TrimSuffix(string(data), "\n")
	if value == "" {
		return "", usageErr{errors.New("the value is read from standard input, which was empty")}
	}
	return value, nil
}

// hasCurrentVersion reports whether a version carries AWSCURRENT, the label GetSecretValue reads,
// so the secret has a value the broker can release.
func hasCurrentVersion(versionsToStages map[string][]string) bool {
	for _, stages := range versionsToStages {
		if slices.Contains(stages, "AWSCURRENT") {
			return true
		}
	}
	return false
}

// restoreBy is the last moment a secret scheduled for deletion can be restored, read back from the
// DeletedDate Secrets Manager answers for it (DescribeSecret's and ListSecrets'); nil while it is
// not scheduled. DeletedDate is the moment the delete ran, and neither call answers the recovery
// window, so this adds the window delete schedules every deletion with, recoveryWindowDays. A
// deletion scheduled elsewhere with a shorter window (the console) is restorable for less than it
// shows. delete itself prints DeleteSecret's own answer, DeletionDate, which is the end of the
// window it scheduled.
func restoreBy(deletedDate *time.Time) *time.Time {
	if deletedDate == nil {
		return nil
	}
	t := deletedDate.Add(recoveryWindowDays * 24 * time.Hour)
	return &t
}

// formatTime is t in UTC as RFC 3339, or "-" when Secrets Manager answered none.
func formatTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// tagMap is tags as a map.
func tagMap(tags []smtypes.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, tag := range tags {
		m[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return m
}

// orDash is s, or "-" when it is empty.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// isAccessDenied reports whether err is AWS's AccessDeniedException, which the SDK answers as a
// generic API error: secretsmanager's types declare no type for it.
func isAccessDenied(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDeniedException"
}

// secretSession is one secret form's connection: the broker, its settings, and the person's AWS
// clients pinned to the broker's region.
type secretSession struct {
	ctx      context.Context
	broker   *client
	settings Settings
	sm       secretsAPI
	st       stsAPI
}

// connectSecrets reads the broker's settings from AGENT_SECRETS_URL and loads the person's AWS
// sign-in (profile, else AWS_PROFILE, else the default chain) in the broker's region.
func connectSecrets(ctx context.Context, profile string) (*secretSession, error) {
	base := strings.TrimSuffix(os.Getenv("AGENT_SECRETS_URL"), "/")
	if base == "" {
		return nil, usageErr{errors.New("AGENT_SECRETS_URL is required")}
	}
	broker := newClient(base)
	settings, err := broker.Settings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the broker's settings: %w", err)
	}
	sm, st, err := awsClients(ctx, profile, settings.AWSRegion)
	if err != nil {
		return nil, err
	}
	return &secretSession{ctx: ctx, broker: broker, settings: settings, sm: sm, st: st}, nil
}

// connectWriter is connectSecrets for a write: it also refuses any sign-in but the person's own in
// the broker's account (requireWriteSignIn) before anything is written, and answers their email.
func connectWriter(ctx context.Context, profile string) (*secretSession, string, error) {
	s, err := connectSecrets(ctx, profile)
	if err != nil {
		return nil, "", err
	}
	email, err := requireWriteSignIn(ctx, s.st, s.settings)
	if err != nil {
		return nil, "", err
	}
	return s, email, nil
}

// id is the Secrets Manager name of the secret whose name under the prefix is slug.
func (s *secretSession) id(slug string) string {
	return s.settings.SecretsPrefix + slug
}

// displayName is the NAME a session asks for the secret named id in Secrets Manager, or id itself
// when its name under the prefix maps to none (the broker refuses such a secret as name-malformed).
func (s *secretSession) displayName(id string) string {
	slug := strings.TrimPrefix(id, s.settings.SecretsPrefix)
	name := policy.SlugToName(slug)
	if back, err := policy.NameToSlug(name); err != nil || back != slug {
		return id
	}
	return name
}

// rereadAfter asks the broker to reread name after a form's write and prints its answer: exit 0
// when the broker now does what the write meant (serves the secret, or, after a delete, does not),
// 1 when its answer contradicts the write or it could not be asked. restorable is when a deleted
// secret stops being restorable, which only delete names.
func rereadAfter(s *secretSession, stdout, stderr io.Writer, form, name string, wantServed bool, restorable *time.Time) int {
	r, err := s.broker.RereadSecret(s.ctx, name)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: the write to Secrets Manager stands, but the broker could not be asked to reread %s, so it serves the change only from its next reload: %v\n", form, name, err)
		return 1
	}
	switch {
	case r.Served:
		fmt.Fprintf(stdout, "broker: serving %s\n", name)
	case !wantServed && r.Reason == policy.ReasonAbsent:
		fmt.Fprintf(stdout, "broker: %s is deleted (restorable until %s)\n", name, formatTime(restorable))
	default:
		fmt.Fprintf(stdout, "broker: refusing %s (reason=%s)\n", name, r.Reason)
	}
	switch {
	case wantServed && !r.Served:
		fmt.Fprintf(stderr, "agent-secrets %s: the broker refuses %s after the write (reason=%s)\n", form, name, r.Reason)
		return 1
	case !wantServed && r.Served:
		fmt.Fprintf(stderr, "agent-secrets %s: the broker still serves %s after its delete\n", form, name)
		return 1
	}
	return 0
}

// secretView is what list and show print of one secret: never its value.
type secretView struct {
	// Name is the NAME a session asks for it as (or its Secrets Manager name, when it maps to none).
	Name string `json:"name"`
	// SecretName is its whole Secrets Manager name.
	SecretName string `json:"secret_name"`
	Owner      string `json:"owner"`
	Tier       string `json:"tier"`
	// HasValue is whether a version carries AWSCURRENT, so the broker can release a value.
	HasValue bool `json:"has_value"`
	// Created and LastChanged are absent when Secrets Manager answers none.
	Created     *time.Time `json:"created,omitempty"`
	LastChanged *time.Time `json:"last_changed,omitempty"`
	// RestoreBy is set while the secret is scheduled for deletion (restoreBy).
	RestoreBy *time.Time `json:"restore_by,omitempty"`
	// Versions maps each version id to its staging labels; only show answers it.
	Versions map[string][]string `json:"versions,omitempty"`
}

// writeIndentedJSON writes v as indented JSON.
func writeIndentedJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdSecretList(args []string, stdout, stderr io.Writer) int {
	const form = "secret list"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	asJSON := flags.Bool("json", false, "print the secrets as JSON")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		return secretFail(stderr, form, usageErr{fmt.Errorf("unexpected argument %q", positional[0])})
	}
	s, err := connectSecrets(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	views := []secretView{}
	pages := secretsmanager.NewListSecretsPaginator(s.sm, &secretsmanager.ListSecretsInput{
		Filters:                []smtypes.Filter{{Key: smtypes.FilterNameStringTypeName, Values: []string{s.settings.SecretsPrefix}}},
		IncludePlannedDeletion: aws.Bool(true),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(s.ctx)
		if err != nil {
			return secretFail(stderr, form, fmt.Errorf("list the agent secrets: %w", err))
		}
		for _, e := range page.SecretList {
			id := aws.ToString(e.Name)
			if !strings.HasPrefix(id, s.settings.SecretsPrefix) {
				continue // the name filter matched more than the namespace
			}
			tags := tagMap(e.Tags)
			views = append(views, secretView{
				Name: s.displayName(id), SecretName: id,
				Owner: tags[policy.TagOwner], Tier: tags[policy.TagTier],
				HasValue: hasCurrentVersion(e.SecretVersionsToStages),
				Created:  e.CreatedDate, LastChanged: e.LastChangedDate,
				RestoreBy: restoreBy(e.DeletedDate),
			})
		}
	}
	// sort.Slice's less answers a bool: an int-returning comparator would be read by broker-refgen
	// as an exit code, which every int-returning function in this package is.
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	if *asJSON {
		if err := writeIndentedJSON(stdout, views); err != nil {
			return secretFail(stderr, form, err)
		}
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tOWNER\tTIER\tVALUE\tDELETION")
	for _, v := range views {
		value := "no"
		if v.HasValue {
			value = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Name, orDash(v.Owner), orDash(v.Tier), value, formatTime(v.RestoreBy))
	}
	if err := tw.Flush(); err != nil {
		return secretFail(stderr, form, err)
	}
	return 0
}

// ---------------------------------------------------------------------------
// show
// ---------------------------------------------------------------------------

func cmdSecretShow(args []string, stdout, stderr io.Writer) int {
	const form = "secret show"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	asJSON := flags.Bool("json", false, "print the secret as JSON")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	_, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, err := connectSecrets(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	out, err := s.sm.DescribeSecret(s.ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(s.id(slug))})
	if err != nil {
		return secretFail(stderr, form, err)
	}
	tags := tagMap(out.Tags)
	view := secretView{
		Name: s.displayName(aws.ToString(out.Name)), SecretName: aws.ToString(out.Name),
		Owner: tags[policy.TagOwner], Tier: tags[policy.TagTier],
		HasValue: hasCurrentVersion(out.VersionIdsToStages),
		Created:  out.CreatedDate, LastChanged: out.LastChangedDate,
		RestoreBy: restoreBy(out.DeletedDate),
		Versions:  out.VersionIdsToStages,
	}
	if *asJSON {
		if err := writeIndentedJSON(stdout, view); err != nil {
			return secretFail(stderr, form, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "name: %s\nsecret_name: %s\nowner: %s\ntier: %s\ncreated: %s\nlast_changed: %s\n",
		view.Name, view.SecretName, orDash(view.Owner), orDash(view.Tier), formatTime(view.Created), formatTime(view.LastChanged))
	if view.RestoreBy != nil {
		fmt.Fprintf(stdout, "deletion: scheduled; restorable until %s\n", formatTime(view.RestoreBy))
	}
	versions := make([]string, 0, len(view.Versions))
	for version := range view.Versions {
		versions = append(versions, version)
	}
	slices.Sort(versions)
	if len(versions) == 0 {
		fmt.Fprintln(stdout, "versions: none")
	}
	for _, version := range versions {
		fmt.Fprintf(stdout, "version: %s %s\n", version, strings.Join(view.Versions[version], ","))
	}
	return 0
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

func cmdSecretCreate(args []string, stdout, stderr io.Writer) int {
	const form = "secret create"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	owner := flags.String("owner", "", "me (your email, from your AWS sign-in) or shared")
	tier := flags.String("tier", "", "agent (its owner's agents use it without asking) or human (a person approves each use)")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err == nil && (*owner == "" || *tier == "") {
		err = usageErr{errors.New("--owner and --tier are required")}
	}
	if err == nil {
		err = checkOwnerTier(*owner, *tier)
	}
	if err != nil {
		return secretFail(stderr, form, err)
	}
	value, err := readSecretValue()
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, email, err := connectWriter(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	ownerValue := ownerTag(*owner, email)
	if _, err := s.sm.CreateSecret(s.ctx, &secretsmanager.CreateSecretInput{
		Name:         aws.String(s.id(slug)),
		KmsKeyId:     aws.String(s.settings.KMSKeyARN),
		SecretString: aws.String(value),
		Tags: []smtypes.Tag{
			{Key: aws.String(policy.TagOwner), Value: aws.String(ownerValue)},
			{Key: aws.String(policy.TagTier), Value: aws.String(*tier)},
		},
	}); err != nil {
		return secretFail(stderr, form, err)
	}
	fmt.Fprintf(stdout, "created %s (owner=%s, tier=%s)\n", name, ownerValue, *tier)
	return rereadAfter(s, stdout, stderr, form, name, true, nil)
}

// ---------------------------------------------------------------------------
// set
// ---------------------------------------------------------------------------

func cmdSecretSet(args []string, stdout, stderr io.Writer) int {
	const form = "secret set"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	value, err := readSecretValue()
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, _, err := connectWriter(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	if _, err := s.sm.PutSecretValue(s.ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String(s.id(slug)), SecretString: aws.String(value),
	}); err != nil {
		return secretFail(stderr, form, err)
	}
	fmt.Fprintf(stdout, "set a new value of %s\n", name)
	return rereadAfter(s, stdout, stderr, form, name, true, nil)
}

// ---------------------------------------------------------------------------
// retag
// ---------------------------------------------------------------------------

// cmdSecretRetag changes a secret's owner, tier or both. It always sends both tags, re-sending the
// one not named as the secret holds it: IAM's TagOwnSecret conditions on both request tags, so a
// request carrying one fails.
func cmdSecretRetag(args []string, stdout, stderr io.Writer) int {
	const form = "secret retag"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	newOwner := flags.String("owner", "", "the new owner: me (your email, from your AWS sign-in) or shared")
	newTier := flags.String("tier", "", "the new tier: agent or human")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err == nil && *newOwner == "" && *newTier == "" {
		err = usageErr{errors.New("--owner, --tier or both is required")}
	}
	if err == nil {
		err = checkOwnerTier(*newOwner, *newTier)
	}
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, email, err := connectWriter(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	id := s.id(slug)
	held, err := s.sm.DescribeSecret(s.ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(id)})
	if err != nil {
		return secretFail(stderr, form, err)
	}
	tags := tagMap(held.Tags)
	owner, tier := tags[policy.TagOwner], tags[policy.TagTier]
	if *newOwner != "" {
		owner = ownerTag(*newOwner, email)
	}
	if *newTier != "" {
		tier = *newTier
	}
	if owner == "" || tier == "" {
		return secretFail(stderr, form, usageErr{fmt.Errorf("%s has no owner tag or no tier tag, and a retag sends both: name the missing one with --owner or --tier", name)})
	}
	if _, err := s.sm.TagResource(s.ctx, &secretsmanager.TagResourceInput{
		SecretId: aws.String(id),
		Tags: []smtypes.Tag{
			{Key: aws.String(policy.TagOwner), Value: aws.String(owner)},
			{Key: aws.String(policy.TagTier), Value: aws.String(tier)},
		},
	}); err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		if isAccessDenied(err) && tags[policy.TagOwner] == policy.OwnerShared {
			fmt.Fprintf(stderr, "agent-secrets %s: %s\n", form, sharedRetagRefused)
		}
		return 1
	}
	fmt.Fprintf(stdout, "retagged %s (owner=%s, tier=%s)\n", name, owner, tier)
	return rereadAfter(s, stdout, stderr, form, name, true, nil)
}

// ---------------------------------------------------------------------------
// delete, restore
// ---------------------------------------------------------------------------

// cmdSecretDelete schedules a secret's deletion with the longest recovery window, never forcing
// it, so restore can bring the secret back until the window ends.
func cmdSecretDelete(args []string, stdout, stderr io.Writer) int {
	const form = "secret delete"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, _, err := connectWriter(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	out, err := s.sm.DeleteSecret(s.ctx, &secretsmanager.DeleteSecretInput{
		SecretId: aws.String(s.id(slug)), RecoveryWindowInDays: aws.Int64(recoveryWindowDays),
	})
	if err != nil {
		return secretFail(stderr, form, err)
	}
	fmt.Fprintf(stdout, "deleted %s\n", name)
	return rereadAfter(s, stdout, stderr, form, name, false, out.DeletionDate)
}

func cmdSecretRestore(args []string, stdout, stderr io.Writer) int {
	const form = "secret restore"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, _, err := connectWriter(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	if _, err := s.sm.RestoreSecret(s.ctx, &secretsmanager.RestoreSecretInput{SecretId: aws.String(s.id(slug))}); err != nil {
		return secretFail(stderr, form, err)
	}
	fmt.Fprintf(stdout, "restored %s\n", name)
	return rereadAfter(s, stdout, stderr, form, name, true, nil)
}
