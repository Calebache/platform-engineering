package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// runWhoami proves the worker's identity and its blast radius (doc.md §17):
// the calls it's allowed to make succeed, and calls outside its role return
// AccessDenied. Run it as a pod with the sre-agent ServiceAccount.
func runWhoami(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	skipModel := fs.Bool("skip-model", false, "don't make the (billed) Bedrock call")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bucket, err := mustEnv("AUDIT_BUCKET")
	if err != nil {
		return err
	}
	bedrockRegion, err := mustEnv("BEDROCK_REGION")
	if err != nil {
		return err
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	var failures []string
	report := func(name string, ok bool, detail string) {
		status := "PASS"
		if !ok {
			status = "FAIL"
			failures = append(failures, name)
		}
		fmt.Printf("%s  %-52s %s\n", status, name, detail)
	}

	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		report("sts:GetCallerIdentity", false, err.Error())
	} else {
		report("sts:GetCallerIdentity", true, aws.ToString(id.Arn))
	}

	s3c := s3.NewFromConfig(cfg)
	key := fmt.Sprintf("audit/whoami/%s.json", time.Now().UTC().Format("20060102T150405Z"))
	_, err = s3c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key),
		Body: bytes.NewReader([]byte(`{"check":"whoami"}`)), ContentType: aws.String("application/json")})
	report("ALLOW s3:PutObject audit/*", err == nil, errText(err, "wrote "+key))

	// Outside the role: each must be refused by IAM.
	_, err = s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	report("DENY  s3:ListBucket on the audit bucket", isAccessDenied(err), errText(err, "unexpectedly allowed"))
	_, err = s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	report("DENY  s3:GetObject (read back its own audit record)", isAccessDenied(err), errText(err, "unexpectedly allowed"))
	_, err = s3c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("outside-audit/x"),
		Body: bytes.NewReader([]byte("x"))})
	report("DENY  s3:PutObject outside audit/", isAccessDenied(err), errText(err, "unexpectedly allowed"))
	_, err = s3c.ListBuckets(ctx, &s3.ListBucketsInput{})
	report("DENY  s3:ListAllMyBuckets", isAccessDenied(err), errText(err, "unexpectedly allowed"))

	if !*skipModel {
		mc, err := bedrock.NewMantleClient(ctx, bedrock.MantleClientConfig{AWSRegion: bedrockRegion})
		if err == nil {
			var msg *anthropic.Message
			msg, err = mc.Messages.New(ctx, anthropic.MessageNewParams{
				Model: anthropic.Model(envOr("MODEL_ID", "anthropic.claude-opus-5-5")), MaxTokens: 64,
				OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffortLow},
				Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Reply with the single word: ready"))},
			})
			if err == nil {
				err = textOf(msg)
			}
		}
		report("ALLOW bedrock-mantle:CreateInference ("+bedrockRegion+")", err == nil, errText(err, "model replied"))

		// The role allows Bedrock only in bedrock_region; any other region is refused.
		if bedrockRegion != "us-west-2" {
			mc2, err := bedrock.NewMantleClient(ctx, bedrock.MantleClientConfig{AWSRegion: "us-west-2"})
			if err == nil {
				_, err = mc2.Messages.New(ctx, anthropic.MessageNewParams{
					Model: anthropic.Model(envOr("MODEL_ID", "anthropic.claude-opus-5-5")), MaxTokens: 16,
					Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("x"))},
				})
			}
			report("DENY  bedrock-mantle:CreateInference (us-west-2)", err != nil && strings.Contains(strings.ToLower(err.Error()), "denied"), errText(err, "unexpectedly allowed"))
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d check(s) failed: %s", len(failures), strings.Join(failures, ", "))
	}
	fmt.Println("identity checks passed: the worker can do exactly what its role allows")
	return nil
}

func isAccessDenied(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		c := apiErr.ErrorCode()
		return c == "AccessDenied" || c == "AccessDeniedException" || c == "Forbidden"
	}
	return false
}

func errText(err error, ok string) string {
	if err == nil {
		return ok
	}
	return truncate(err.Error(), 140)
}

func textOf(m *anthropic.Message) error {
	for _, b := range m.Content {
		if t, ok := b.AsAny().(anthropic.TextBlock); ok && t.Text != "" {
			return nil
		}
	}
	return fmt.Errorf("no text in reply (stop_reason=%s)", m.StopReason)
}
