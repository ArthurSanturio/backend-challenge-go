// Package brokerauth configures SQS as an internal, trusted transport.
// Provider identities are authenticated before a trusted service publishes.
package brokerauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsarn "github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/junglegaming/backend-challenge-go/internal/sqsconsumer"
)

type Principals struct {
	Administrator, Runtime, TrustedPublisher, EventConsumer string
}

type statement struct {
	Effect    string                         `json:"Effect"`
	Action    []string                       `json:"Action"`
	Resource  []string                       `json:"Resource"`
	Principal any                            `json:"Principal,omitempty"`
	Condition map[string]map[string][]string `json:"Condition,omitempty"`
}

func policy(statements ...statement) string {
	encoded, _ := json.Marshal(struct {
		Version   string      `json:"Version"`
		Statement []statement `json:"Statement"`
	}{"2012-10-17", statements})
	return string(encoded)
}

var inspect = []string{"sqs:GetQueueUrl", "sqs:GetQueueAttributes"}
var consume = []string{"sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility"}
var administer = []string{"sqs:SetQueueAttributes", "sqs:AddPermission", "sqs:RemovePermission", "sqs:DeleteQueue", "sqs:PurgeQueue", "sqs:TagQueue", "sqs:UntagQueue", "sqs:StartMessageMoveTask", "sqs:CancelMessageMoveTask"}

type Queues map[string]string

// Discover obtains the real ARNs, without REGION/ACCOUNT_ID placeholders.
func Discover(ctx context.Context, client *sqs.Client) (Queues, error) {
	queues := Queues{}
	for _, name := range []string{sqsconsumer.QueueName, sqsconsumer.DLQName, sqsconsumer.EventQueueName, sqsconsumer.EventDLQName} {
		queue, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err != nil {
			return nil, err
		}
		attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: queue.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
		if err != nil {
			return nil, err
		}
		arn := attrs.Attributes["QueueArn"]
		parsed, parseErr := awsarn.Parse(arn)
		if parseErr != nil || parsed.Service != "sqs" || parsed.Region == "" || len(parsed.AccountID) != 12 || parsed.Resource != name {
			return nil, errors.New("SQS returned an invalid queue ARN")
		}
		queues[name] = arn
	}
	return queues, nil
}

// IdentityPolicies grants only the operations each internal service needs.
func IdentityPolicies(q Queues) map[string]string {
	input, inputDLQ := q[sqsconsumer.QueueName], q[sqsconsumer.DLQName]
	events, eventsDLQ := q[sqsconsumer.EventQueueName], q[sqsconsumer.EventDLQName]
	allow := func(actions, resources []string) statement {
		return statement{Effect: "Allow", Action: actions, Resource: resources}
	}
	return map[string]string{
		"runtime":   policy(allow(inspect, []string{input, inputDLQ, events, eventsDLQ}), allow(consume, []string{input}), allow([]string{"sqs:SendMessage"}, []string{events, eventsDLQ})),
		"publisher": policy(allow(inspect, []string{input}), allow([]string{"sqs:SendMessage"}, []string{input})),
		"events":    policy(allow(inspect, []string{events, eventsDLQ}), allow(consume, []string{events})),
	}
}

// Apply protects the queues even if another same-account identity is accidentally
// given broad sqs:* permissions. Native DLQ redrive is controlled separately by
// RedriveAllowPolicy; these restrictions govern callers of the SQS APIs.
func Apply(ctx context.Context, client *sqs.Client, principals Principals) error {
	for _, arn := range []string{principals.Administrator, principals.Runtime, principals.TrustedPublisher, principals.EventConsumer} {
		if !validPrincipal(arn) {
			return errors.New("all SQS principal ARNs must identify an IAM role, user or account root")
		}
	}
	for _, arn := range []string{principals.Runtime, principals.TrustedPublisher, principals.EventConsumer} {
		if strings.HasSuffix(arn, ":root") {
			return errors.New("SQS services must use IAM roles or users, never account root")
		}
	}
	seen := map[string]bool{}
	for _, principal := range []string{principals.Administrator, principals.Runtime, principals.TrustedPublisher, principals.EventConsumer} {
		if seen[principal] {
			return errors.New("SQS administrator, runtime, publisher and event consumer identities must be separate")
		}
		seen[principal] = true
	}
	queues, err := Discover(ctx, client)
	if err != nil {
		return err
	}
	for name, arn := range queues {
		parts := strings.Split(arn, ":")
		ownerRoot := "arn:" + parts[1] + ":iam::" + parts[4] + ":root"
		sender, receiver := principals.Administrator, principals.Administrator
		switch name {
		case sqsconsumer.QueueName:
			sender, receiver = principals.TrustedPublisher, principals.Runtime
		case sqsconsumer.EventQueueName:
			sender, receiver = principals.Runtime, principals.EventConsumer
		case sqsconsumer.EventDLQName:
			sender = principals.Runtime
		}
		deny := func(actions []string, allowed ...string) statement {
			return statement{Effect: "Deny", Action: actions, Resource: []string{arn}, Principal: "*", Condition: map[string]map[string][]string{"ArnNotEquals": {"aws:PrincipalArn": allowed}}}
		}
		sendDeny := deny([]string{"sqs:SendMessage"}, sender, principals.Administrator)
		var nativeRedrive *statement
		source := ""
		switch name {
		case sqsconsumer.DLQName:
			source = queues[sqsconsumer.QueueName]
		case sqsconsumer.EventDLQName:
			source = queues[sqsconsumer.EventQueueName]
		}
		if source != "" {
			// SQS's native transfer to a DLQ is a service call identified by
			// SourceArn, not by one of our IAM runtime/publisher principals.
			sendDeny.Condition["ArnNotEquals"]["aws:SourceArn"] = []string{source}
			nativeRedrive = &statement{Effect: "Allow", Action: []string{"sqs:SendMessage"}, Resource: []string{arn}, Principal: map[string]string{"Service": "sqs.amazonaws.com"}, Condition: map[string]map[string][]string{"ArnEquals": {"aws:SourceArn": {source}}}}
		}
		statements := []statement{
			statement{Effect: "Allow", Action: inspect, Resource: []string{arn}, Principal: map[string][]string{"AWS": {principals.Runtime, sender, receiver, principals.Administrator}}},
			statement{Effect: "Allow", Action: []string{"sqs:SendMessage"}, Resource: []string{arn}, Principal: map[string][]string{"AWS": {sender}}},
			statement{Effect: "Allow", Action: consume, Resource: []string{arn}, Principal: map[string][]string{"AWS": {receiver}}},
			// IAM uses the single-message permissions for batch operations too.
			sendDeny,
			deny(consume, receiver, principals.Administrator),
			// Keep the owner's root identity available for administrative recovery.
			deny(administer, principals.Administrator, ownerRoot),
		}
		if nativeRedrive != nil {
			statements = append(statements, *nativeRedrive)
		}
		body := policy(statements...)
		queue, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err != nil {
			return err
		}
		if _, err := client.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{QueueUrl: queue.QueueUrl, Attributes: map[string]string{"Policy": body}}); err != nil {
			return fmt.Errorf("apply SQS access policy for %s: %w", name, err)
		}
	}
	return nil
}

func validPrincipal(value string) bool {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "iam" || parts[3] != "" || len(parts[4]) != 12 || strings.ContainsAny(value, "*? \t\r\n") {
		return false
	}
	for _, c := range parts[4] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return parts[5] == "root" || strings.HasPrefix(parts[5], "role/") && len(parts[5]) > 5 || strings.HasPrefix(parts[5], "user/") && len(parts[5]) > 5
}
