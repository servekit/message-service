package send

import (
	"testing"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	"github.com/servekit/message-service/internal/store/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRender(t *testing.T) {
	params := map[string]string{"code": "123456", "minutes": "10"}
	assert.Equal(t,
		"Your code is 123456, valid for 10 minutes.",
		Render("Your code is {{code}}, valid for {{minutes}} minutes.", params))
	// spaces inside braces tolerated
	assert.Equal(t, "x=1", Render("x={{ n }}", map[string]string{"n": "1"}))
	// unknown params render empty; empty params returns input
	assert.Equal(t, "a={{b}}", Render("a={{b}}", nil))
	// no placeholders
	assert.Equal(t, "plain", Render("plain", params))
}

func TestValidateParams(t *testing.T) {
	specs := []models.TemplateParamSpec{
		{Name: "code", Required: true},
		{Name: "nickname"},
	}
	require.NoError(t, ValidateParams(specs, map[string]string{"code": "1", "extra": "x"}))
	err := ValidateParams(specs, map[string]string{"nickname": "a"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "code")
}

func TestParseAndMarshalParamSpecs(t *testing.T) {
	specs := []models.TemplateParamSpec{{Name: "code", Required: true, Description: "verification code"}}
	raw, err := MarshalParamSpecs(specs)
	require.NoError(t, err)
	parsed, err := ParseParamSpecs(raw)
	require.NoError(t, err)
	assert.Equal(t, specs, parsed)

	empty, err := ParseParamSpecs(nil)
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestRoutesRoundTrip(t *testing.T) {
	routes := []Route{
		{AccountID: 1, SignatureID: 10, Weight: 3},
		{AccountID: 2, SignatureID: 20},
	}
	raw, err := MarshalRoutes(routes)
	require.NoError(t, err)
	parsed, err := ParseRoutes(raw)
	require.NoError(t, err)
	assert.Equal(t, routes, parsed)
}

func TestOrderRoutes(t *testing.T) {
	routes := []Route{
		{AccountID: 1, Weight: 1},
		{AccountID: 2, Weight: 1},
		{AccountID: 3, Weight: 1},
	}
	// 200 draws: every ordering starts with a member and preserves the rest
	// in list order (minus the start).
	for i := 0; i < 200; i++ {
		ordered := OrderRoutes(routes)
		require.Len(t, ordered, 3)
		start := ordered[0].AccountID
		rest := []int64{ordered[1].AccountID, ordered[2].AccountID}
		var expectedRest []int64
		for _, r := range routes {
			if r.AccountID != start {
				expectedRest = append(expectedRest, r.AccountID)
			}
		}
		assert.Equal(t, expectedRest, rest)
	}

	// weight <= 0 treated as 1; empty input returns nil
	assert.Nil(t, OrderRoutes(nil))
	ordered := OrderRoutes([]Route{{AccountID: 7, Weight: -5}})
	require.Len(t, ordered, 1)
	assert.Equal(t, int64(7), ordered[0].AccountID)
}

func TestContentRoundTripAndShapeValidation(t *testing.T) {
	// email
	raw, err := MarshalEmailContent(&EmailContent{Subject: "s {{code}}", TextBody: "b {{code}}", HTMLBody: "<p>{{code}}</p>"})
	require.NoError(t, err)
	require.NoError(t, ValidateContentShape(
		pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL, pb.TemplateKind_TEMPLATE_KIND_EMAIL_RENDER, raw))
	c, err := DecodeEmailContent(raw)
	require.NoError(t, err)
	assert.Equal(t, "s {{code}}", c.Subject)

	// vendor codes
	raw, err = MarshalVendorCodes([]VendorCode{
		{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, TemplateCode: "SMS_1"},
		{Vendor: pb.SmsVendor_SMS_VENDOR_TENCENT, TemplateCode: "14xxxx"},
	})
	require.NoError(t, err)
	require.NoError(t, ValidateContentShape(
		pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES, raw))
	codes, err := DecodeVendorCodes(raw)
	require.NoError(t, err)
	assert.Equal(t, "SMS_1", codes[pb.SmsVendor_SMS_VENDOR_ALIYUN])
	assert.Len(t, SortVendorCodes(codes), 2)

	// sms content
	raw, err = MarshalSmsContent(&SmsContent{Content: "code {{code}}"})
	require.NoError(t, err)
	require.NoError(t, ValidateContentShape(
		pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, pb.TemplateKind_TEMPLATE_KIND_SMS_CONTENT, raw))
	sc, err := DecodeSmsContent(raw)
	require.NoError(t, err)
	assert.Equal(t, "code {{code}}", sc.Content)

	// mismatched pairings are rejected
	emailRaw, _ := MarshalEmailContent(&EmailContent{Subject: "s", TextBody: "b"})
	err = ValidateContentShape(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES, emailRaw)
	assert.Error(t, err)
	err = ValidateContentShape(
		pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL, pb.TemplateKind_TEMPLATE_KIND_EMAIL_RENDER, models.RawJSON("{}"))
	assert.Error(t, err)
}
