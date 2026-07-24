package dal

import (
	"context"
	"testing"

	"github.com/servekit/message-service/internal/store/models"

	"github.com/servekit/go-common/dbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupEmailAttachmentDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.MessageEmailRecord{}, &models.MessageEmailRecordAttachment{}))
	return db
}

func TestCreateEmailRecordAttachments(t *testing.T) {
	db := setupEmailAttachmentDB(t)
	ctx := context.Background()

	atts := []*models.MessageEmailRecordAttachment{
		{
			EmailRecordID: 100,
			Filename:      "report.pdf",
			URL:           "https://oss.example.com/report.pdf",
			Inline:        false,
			MimeType:      "application/pdf",
			SizeBytes:     1024,
		},
		{
			EmailRecordID: 100,
			Filename:      "logo.png",
			URL:           "https://oss.example.com/logo.png",
			Inline:        true,
			MimeType:      "image/png",
		},
	}
	require.NoError(t, CreateEmailRecordAttachments(ctx, db, atts))

	found, err := ListEmailRecordAttachments(ctx, db, 100)
	require.NoError(t, err)
	assert.Len(t, found, 2)
	assert.Equal(t, "report.pdf", found[0].Filename)
	assert.Equal(t, "logo.png", found[1].Filename)
}

func TestListEmailRecordAttachments_empty(t *testing.T) {
	db := setupEmailAttachmentDB(t)
	ctx := context.Background()

	found, err := ListEmailRecordAttachments(ctx, db, 999)
	require.NoError(t, err)
	assert.Empty(t, found)
}

func TestCreateEmailRecordAttachments_emptyIsNoop(t *testing.T) {
	db := setupEmailAttachmentDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecordAttachments(ctx, db, nil))
	require.NoError(t, CreateEmailRecordAttachments(ctx, db, []*models.MessageEmailRecordAttachment{}))

	found, err := ListEmailRecordAttachments(ctx, db, 1)
	require.NoError(t, err)
	assert.Empty(t, found)
}

func TestListEmailRecordAttachments_ordersByIDAsc(t *testing.T) {
	db := setupEmailAttachmentDB(t)
	ctx := context.Background()

	// Insert in non-sorted order; verify retrieval is by ID asc.
	require.NoError(t, CreateEmailRecordAttachments(ctx, db, []*models.MessageEmailRecordAttachment{
		{EmailRecordID: 200, Filename: "z.pdf", URL: "https://x/z"},
		{EmailRecordID: 200, Filename: "a.pdf", URL: "https://x/a"},
		{EmailRecordID: 200, Filename: "m.pdf", URL: "https://x/m"},
	}))

	found, err := ListEmailRecordAttachments(ctx, db, 200)
	require.NoError(t, err)
	require.Len(t, found, 3)
	// IDs should be ascending; filenames match insertion order (z, a, m).
	assert.Equal(t, "z.pdf", found[0].Filename)
	assert.Equal(t, "a.pdf", found[1].Filename)
	assert.Equal(t, "m.pdf", found[2].Filename)
	for i := 1; i < len(found); i++ {
		assert.Less(t, found[i-1].ID, found[i].ID, "IDs must be ascending")
	}
}

func TestListEmailRecordAttachmentsByEmailRecordIDs_emptyInput(t *testing.T) {
	db := setupEmailAttachmentDB(t)
	ctx := context.Background()

	out, err := ListEmailRecordAttachmentsByEmailRecordIDs(ctx, db, nil)
	require.NoError(t, err)
	assert.Empty(t, out, "nil input must return empty map without touching DB")

	out2, err := ListEmailRecordAttachmentsByEmailRecordIDs(ctx, db, []int64{})
	require.NoError(t, err)
	assert.Empty(t, out2, "empty slice input must return empty map")
}

func TestListEmailRecordAttachmentsByEmailRecordIDs_groupsAndOrders(t *testing.T) {
	db := setupEmailAttachmentDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecordAttachments(ctx, db, []*models.MessageEmailRecordAttachment{
		{EmailRecordID: 10, Filename: "10-a", URL: "https://x/10a"},
		{EmailRecordID: 30, Filename: "30-a", URL: "https://x/30a"},
		{EmailRecordID: 10, Filename: "10-b", URL: "https://x/10b"},
		{EmailRecordID: 20, Filename: "20-a", URL: "https://x/20a"},
	}))

	// Query with IDs out of order; result map groups by email_record_id and
	// each slice is ordered by row id ascending.
	out, err := ListEmailRecordAttachmentsByEmailRecordIDs(ctx, db, []int64{20, 10, 30})
	require.NoError(t, err)
	require.Len(t, out, 3)

	require.Len(t, out[10], 2)
	assert.Equal(t, "10-a", out[10][0].Filename)
	assert.Equal(t, "10-b", out[10][1].Filename)

	require.Len(t, out[20], 1)
	assert.Equal(t, "20-a", out[20][0].Filename)

	require.Len(t, out[30], 1)
	assert.Equal(t, "30-a", out[30][0].Filename)
}

func TestListEmailRecordAttachmentsByEmailRecordIDs_missingID(t *testing.T) {
	db := setupEmailAttachmentDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecordAttachments(ctx, db, []*models.MessageEmailRecordAttachment{
		{EmailRecordID: 10, Filename: "10-a", URL: "https://x/10a"},
	}))

	// ID 99 has no rows — it should not appear as a key in the map.
	out, err := ListEmailRecordAttachmentsByEmailRecordIDs(ctx, db, []int64{10, 99})
	require.NoError(t, err)
	require.Contains(t, out, int64(10))
	assert.NotContains(t, out, int64(99), "missing IDs should not produce map entries")
}

// TestCreateEmailRecordAttachments_inlineContentEmptyURL verifies the URL
// column accepts empty strings for inline-content attachments (no NOT NULL
// constraint).
func TestCreateEmailRecordAttachments_inlineContentEmptyURL(t *testing.T) {
	db := setupEmailAttachmentDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecordAttachments(ctx, db, []*models.MessageEmailRecordAttachment{
		{EmailRecordID: 300, Filename: "inline.txt", URL: "", MimeType: "text/plain"},
	}))

	found, err := ListEmailRecordAttachments(ctx, db, 300)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "", found[0].URL, "inline-content attachment has empty URL")
	assert.Equal(t, "inline.txt", found[0].Filename)
}
