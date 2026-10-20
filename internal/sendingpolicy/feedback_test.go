package sendingpolicy_test

import (
	"testing"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// TestDeriveBucket pins the bucket a full event kind + subtype maps to, and
// which of them repair the suppression list. Suppression-list subtypes are
// excluded from accounting (SES never attempted delivery) but still repair;
// the global-list subtype Suppressed stays a hard bounce.
func TestDeriveBucket(t *testing.T) {
	cases := []struct {
		name          string
		kind          delivery.EventKind
		btype, bsub   string
		csub          string
		bucket        sendingpolicy.Bucket
		repair        bool
		source        string
		wantRankAbove sendingpolicy.Bucket // must rank strictly above this bucket
	}{
		{"delivered", delivery.KindDelivery, "", "", "", sendingpolicy.BucketDelivered, false, "", sendingpolicy.BucketNone},
		{"hard bounce general", delivery.KindBounce, "permanent", "General", "", sendingpolicy.BucketHardBounce, true, "bounce", sendingpolicy.BucketTerminalOther},
		{"hard bounce no email", delivery.KindBounce, "permanent", "NoEmail", "", sendingpolicy.BucketHardBounce, true, "bounce", sendingpolicy.BucketTerminalOther},
		{"global suppressed counts", delivery.KindBounce, "permanent", "Suppressed", "", sendingpolicy.BucketHardBounce, true, "bounce", sendingpolicy.BucketTerminalOther},
		{"account list excluded but repairs", delivery.KindBounce, "permanent", "OnAccountSuppressionList", "", sendingpolicy.BucketNone, true, "bounce", ""},
		{"tenant list excluded but repairs", delivery.KindBounce, "permanent", "OnTenantSuppressionList", "", sendingpolicy.BucketNone, true, "bounce", ""},
		{"transient bounce is terminal other", delivery.KindBounce, "transient", "MailboxFull", "", sendingpolicy.BucketTerminalOther, false, "", sendingpolicy.BucketDelivered},
		{"undetermined bounce is terminal other", delivery.KindBounce, "undetermined", "", "", sendingpolicy.BucketTerminalOther, false, "", sendingpolicy.BucketDelivered},
		{"genuine complaint", delivery.KindComplaint, "", "", "", sendingpolicy.BucketComplaint, true, "complaint", sendingpolicy.BucketHardBounce},
		{"complaint on account list excluded but repairs", delivery.KindComplaint, "", "", "OnAccountSuppressionList", sendingpolicy.BucketNone, true, "complaint", ""},
		{"delay is nothing", delivery.KindDeliveryDelay, "", "", "", sendingpolicy.BucketNone, false, "", ""},
		{"send is nothing", delivery.KindSend, "", "", "", sendingpolicy.BucketNone, false, "", ""},
		{"reject is nothing", delivery.KindReject, "", "", "", sendingpolicy.BucketNone, false, "", ""},
		{"other is nothing", delivery.KindOther, "", "", "", sendingpolicy.BucketNone, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := sendingpolicy.DeriveBucket(tc.kind, tc.btype, tc.bsub, tc.csub)
			if d.Bucket != tc.bucket || d.Repair != tc.repair || d.Source != tc.source {
				t.Fatalf("got bucket=%s repair=%v source=%q, want %s/%v/%q", d.Bucket, d.Repair, d.Source, tc.bucket, tc.repair, tc.source)
			}
			if tc.wantRankAbove != "" && d.Bucket.Rank() <= tc.wantRankAbove.Rank() {
				t.Fatalf("%s rank %d must exceed %s rank %d", d.Bucket, d.Bucket.Rank(), tc.wantRankAbove, tc.wantRankAbove.Rank())
			}
		})
	}
}

// TestBucketRankOrder pins the exact evidence order the monotonic rule uses.
func TestBucketRankOrder(t *testing.T) {
	order := []sendingpolicy.Bucket{sendingpolicy.BucketNone, sendingpolicy.BucketDelivered, sendingpolicy.BucketTerminalOther, sendingpolicy.BucketHardBounce, sendingpolicy.BucketComplaint}
	for i, b := range order {
		if b.Rank() != i {
			t.Fatalf("%s rank = %d, want %d", b, b.Rank(), i)
		}
	}
}
