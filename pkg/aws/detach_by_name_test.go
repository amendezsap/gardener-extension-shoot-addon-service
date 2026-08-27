package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"
)

// fakeIAM implements iamAPI for testing DetachRolePolicyByName.
type fakeIAM struct {
	// pages returned by ListAttachedRolePolicies, in order.
	pages       []*iam.ListAttachedRolePoliciesOutput
	listErr     error
	detachErr   error
	detachedARN string // records the ARN passed to DetachRolePolicy
	listCalls   int
}

func (f *fakeIAM) AttachRolePolicy(ctx context.Context, in *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	return &iam.AttachRolePolicyOutput{}, nil
}
func (f *fakeIAM) DetachRolePolicy(ctx context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	if f.detachErr != nil {
		return nil, f.detachErr
	}
	f.detachedARN = aws.ToString(in.PolicyArn)
	return &iam.DetachRolePolicyOutput{}, nil
}
func (f *fakeIAM) ListAttachedRolePolicies(ctx context.Context, in *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	i := f.listCalls
	f.listCalls++
	if i < len(f.pages) {
		return f.pages[i], nil
	}
	return &iam.ListAttachedRolePoliciesOutput{}, nil
}

func attached(name, arn string) iamtypes.AttachedPolicy {
	return iamtypes.AttachedPolicy{PolicyName: aws.String(name), PolicyArn: aws.String(arn)}
}

type apiErr struct{ code string }

func (e *apiErr) Error() string                 { return e.code }
func (e *apiErr) ErrorCode() string             { return e.code }
func (e *apiErr) ErrorMessage() string          { return e.code }
func (e *apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestDetachRolePolicyByName(t *testing.T) {
	const target = "external-agents-policy"
	const targetARN = "arn:aws-us-gov:iam::123456789012:policy/global/external-agents-policy"

	t.Run("detaches matching policy by resolved ARN", func(t *testing.T) {
		f := &fakeIAM{pages: []*iam.ListAttachedRolePoliciesOutput{{
			AttachedPolicies: []iamtypes.AttachedPolicy{
				attached("CloudWatchAgentServerPolicy", "arn:aws-us-gov:iam::aws:policy/CloudWatchAgentServerPolicy"),
				attached(target, targetARN),
			},
		}}}
		c := &Client{iam: f}
		detached, err := c.DetachRolePolicyByName(context.Background(), "shoot--p--x-nodes", target)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !detached {
			t.Fatal("expected detached=true")
		}
		if f.detachedARN != targetARN {
			t.Fatalf("detached wrong ARN: %q", f.detachedARN)
		}
	})

	t.Run("no-op when policy absent", func(t *testing.T) {
		f := &fakeIAM{pages: []*iam.ListAttachedRolePoliciesOutput{{
			AttachedPolicies: []iamtypes.AttachedPolicy{
				attached("AmazonSSMManagedInstanceCore", "arn:aws-us-gov:iam::aws:policy/AmazonSSMManagedInstanceCore"),
			},
		}}}
		c := &Client{iam: f}
		detached, err := c.DetachRolePolicyByName(context.Background(), "shoot--p--x-nodes", target)
		if err != nil || detached {
			t.Fatalf("expected (false,nil), got (%v,%v)", detached, err)
		}
		if f.detachedARN != "" {
			t.Fatalf("should not have detached anything, got %q", f.detachedARN)
		}
	})

	t.Run("paginates", func(t *testing.T) {
		f := &fakeIAM{pages: []*iam.ListAttachedRolePoliciesOutput{
			{AttachedPolicies: []iamtypes.AttachedPolicy{attached("Other", "arn:x")}, IsTruncated: true, Marker: aws.String("m1")},
			{AttachedPolicies: []iamtypes.AttachedPolicy{attached(target, targetARN)}},
		}}
		c := &Client{iam: f}
		detached, err := c.DetachRolePolicyByName(context.Background(), "shoot--p--x-nodes", target)
		if err != nil || !detached {
			t.Fatalf("expected (true,nil), got (%v,%v)", detached, err)
		}
		if f.listCalls != 2 {
			t.Fatalf("expected 2 list calls, got %d", f.listCalls)
		}
	})

	t.Run("role gone -> no-op", func(t *testing.T) {
		f := &fakeIAM{listErr: &apiErr{code: "NoSuchEntity"}}
		c := &Client{iam: f}
		detached, err := c.DetachRolePolicyByName(context.Background(), "gone-nodes", target)
		if err != nil || detached {
			t.Fatalf("expected (false,nil) for missing role, got (%v,%v)", detached, err)
		}
	})

	t.Run("list error surfaces", func(t *testing.T) {
		f := &fakeIAM{listErr: errors.New("boom")}
		c := &Client{iam: f}
		if _, err := c.DetachRolePolicyByName(context.Background(), "r-nodes", target); err == nil {
			t.Fatal("expected error")
		}
	})
}
