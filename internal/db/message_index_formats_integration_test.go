package db

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMessageIndexConsolidationWithConversationFormats(t *testing.T) {
	for _, refs := range []bool{false, true} {
		for _, digests := range []bool{false, true} {
			t.Run(fmt.Sprintf("refs_%t_digests_%t", refs, digests), func(t *testing.T) {
				d := legacyMessageIndexDB(t)
				storageFormatsSeed(t, d, true)
				// The fixture has an old index layout but current generated
				// records. Finish its synthetic parser-version initialization.
				require.NoError(t, d.setDataVersion(t.Context()))
				require.NoError(t, d.EnableArchiveOnly(t.Context()))
				if refs {
					storageFormatsSet(t, d, "refs", true)
				}
				if digests {
					storageFormatsSet(t, d, "digests", true)
				}
				history := storageFormatsSnapshot(t, d)
				state := sessionRefArchiveState(t.Context(), t, d)
				stored, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
				require.NoError(t, err)
				visible, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible"})
				require.NoError(t, err)
				messages, err := d.GetAllMessages(t.Context(), "chat")
				require.NoError(t, err)
				require.Len(t, messages, 1)
				_, err = d.PinMessage(t.Context(), "chat", messages[0].ID, nil)
				require.NoError(t, err)
				result, err := d.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir(), ConsolidateMessageIndex: true})
				require.NoError(t, err)
				assert.Equal(t, "consolidated", result.MessageIndexStatus)
				assert.Equal(t, history, storageFormatsSnapshot(t, d))
				assert.Equal(t, state, sessionRefArchiveState(t.Context(), t, d))
				storageFormatsRequirePhysical(t, d, refs, digests, true)
				storageFormatsAssertPinned(t, d, stored, visible)
				pins, err := d.ListPinnedMessages(t.Context(), "chat", "")
				require.NoError(t, err)
				require.Len(t, pins, 1)
				assert.Equal(t, messages[0].ID, pins[0].MessageID)
				path := d.Path()
				require.NoError(t, d.Close())
				reopened, err := OpenForMessageIndexConsolidation(t.Context(), path)
				require.NoError(t, err)
				defer reopened.Close()
				again, err := reopened.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir(), ConsolidateMessageIndex: true})
				require.NoError(t, err)
				assert.Equal(t, "already_canonical", again.MessageIndexStatus)
				assert.Equal(t, history, storageFormatsSnapshot(t, reopened))
				storageFormatsRequirePhysical(t, reopened, refs, digests, true)
				if digests {
					storageFormatsSet(t, reopened, "digests", false)
				}
				if refs {
					storageFormatsSet(t, reopened, "refs", false)
				}
				assert.Equal(t, history, storageFormatsSnapshot(t, reopened))
				storageFormatsRequirePhysical(t, reopened, false, false, true)
				storageFormatsAssertPinned(t, reopened, stored, visible)
			})
		}
	}
}
