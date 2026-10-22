package brokerauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type Identity struct {
	ARN         string
	Credentials aws.Credentials
}
type LocalIdentities struct {
	Runtime, Publisher, Events Identity
}

// LocalIAM refuses AWS endpoints: IAM users/access keys here are emulator-only.
// Production services use externally managed IAM roles and Apply.
func LocalIAM(client *sqs.Client) (*iam.Client, error) {
	options := client.Options()
	if options.BaseEndpoint == nil {
		return nil, errors.New("local IAM setup requires a local emulator endpoint")
	}
	endpoint, err := url.Parse(*options.BaseEndpoint)
	if err != nil {
		return nil, err
	}
	switch endpoint.Hostname() {
	case "localhost", "127.0.0.1", "::1", "ministack":
	default:
		return nil, errors.New("local IAM users may only be created on the local emulator")
	}
	return iam.NewFromConfig(aws.Config{Region: options.Region, Credentials: options.Credentials, HTTPClient: options.HTTPClient}, func(o *iam.Options) { o.BaseEndpoint = options.BaseEndpoint }), nil
}

// SetupLocal creates real emulator IAM users, installs their policies and saves
// separate credential files. Existing keys are reused when the broker still has
// them, so repeating setup does not invalidate a running application's identity.
func SetupLocal(ctx context.Context, client *sqs.Client, directory string) (LocalIdentities, error) {
	admin, err := LocalIAM(client)
	if err != nil {
		return LocalIdentities{}, err
	}
	queues, err := Discover(ctx, client)
	if err != nil {
		return LocalIdentities{}, err
	}
	policies := IdentityPolicies(queues)
	var identities LocalIdentities
	for role, destination := range map[string]*Identity{"runtime": &identities.Runtime, "publisher": &identities.Publisher, "events": &identities.Events} {
		identity, err := localIdentity(ctx, admin, "wallet-"+role, policies[role], filepath.Join(directory, role))
		if err != nil {
			return LocalIdentities{}, fmt.Errorf("configure local SQS %s identity: %w", role, err)
		}
		*destination = identity
	}
	arnParts := strings.Split(identities.Runtime.ARN, ":")
	principals := Principals{Administrator: "arn:" + arnParts[1] + ":iam::" + arnParts[4] + ":root", Runtime: identities.Runtime.ARN, TrustedPublisher: identities.Publisher.ARN, EventConsumer: identities.Events.ARN}
	if err := Apply(ctx, client, principals); err != nil {
		return LocalIdentities{}, err
	}
	return identities, nil
}

func localIdentity(ctx context.Context, client *iam.Client, name, document, directory string) (Identity, error) {
	created, err := client.CreateUser(ctx, &iam.CreateUserInput{UserName: aws.String(name)})
	var existing *iamtypes.EntityAlreadyExistsException
	var user *iamtypes.User
	if errors.As(err, &existing) {
		found, getErr := client.GetUser(ctx, &iam.GetUserInput{UserName: aws.String(name)})
		if getErr != nil {
			return Identity{}, getErr
		}
		user = found.User
	} else if err != nil {
		return Identity{}, err
	} else {
		user = created.User
	}
	if user == nil || !validPrincipal(aws.ToString(user.Arn)) {
		return Identity{}, errors.New("IAM did not return a user identity")
	}
	if _, err := client.PutUserPolicy(ctx, &iam.PutUserPolicyInput{UserName: aws.String(name), PolicyName: aws.String("wallet-sqs"), PolicyDocument: aws.String(document)}); err != nil {
		return Identity{}, err
	}
	identity := Identity{ARN: aws.ToString(user.Arn)}
	saved, readErr := os.ReadFile(filepath.Join(directory, "identity.json"))
	if readErr != nil && !os.IsNotExist(readErr) {
		return Identity{}, readErr
	}
	var previous Identity
	if len(saved) != 0 && json.Unmarshal(saved, &previous) != nil {
		return Identity{}, errors.New("invalid saved local IAM identity")
	}
	keys, err := client.ListAccessKeys(ctx, &iam.ListAccessKeysInput{UserName: aws.String(name)})
	if err != nil {
		return Identity{}, err
	}
	for _, key := range keys.AccessKeyMetadata {
		if previous.ARN == identity.ARN && previous.Credentials.AccessKeyID == aws.ToString(key.AccessKeyId) && previous.Credentials.SecretAccessKey != "" && key.Status == iamtypes.StatusTypeActive {
			return previous, saveIdentity(directory, previous)
		}
	}
	// These users belong exclusively to this local bootstrap; lost credentials
	// are replaced when the emulator or its credential volumes are recreated.
	for _, key := range keys.AccessKeyMetadata {
		if _, err := client.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{UserName: aws.String(name), AccessKeyId: key.AccessKeyId}); err != nil {
			return Identity{}, err
		}
	}
	key, err := client.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: aws.String(name)})
	if err != nil {
		return Identity{}, err
	}
	if key.AccessKey == nil || aws.ToString(key.AccessKey.AccessKeyId) == "" || aws.ToString(key.AccessKey.SecretAccessKey) == "" {
		return Identity{}, errors.New("IAM did not return access credentials")
	}
	identity.Credentials = aws.Credentials{AccessKeyID: aws.ToString(key.AccessKey.AccessKeyId), SecretAccessKey: aws.ToString(key.AccessKey.SecretAccessKey)}
	return identity, saveIdentity(directory, identity)
}

func saveIdentity(directory string, identity Identity) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(directory, "identity.json"), encoded); err != nil {
		return err
	}
	credentials := fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n", identity.Credentials.AccessKeyID, identity.Credentials.SecretAccessKey)
	return writePrivate(filepath.Join(directory, "credentials"), []byte(credentials))
}

func writePrivate(path string, contents []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
