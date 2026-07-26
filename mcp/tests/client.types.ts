import type { McpClient } from "../src/client.js";
import type { McpOutput } from "../src/tools/util.js";

type ListMessagesParams = Parameters<McpClient["listMessages"]>[0];

const senderFilter: ListMessagesParams = { from_: "alice@example.com" };
void senderFilter;

// The MCP wrapper must preserve generated SDK q filters while adding only its
// cursor/address transport fields. This assignment fails to typecheck if the
// handwritten wrapper parameter shape drifts from the SDK again.
const queryFilter: ListMessagesParams = { q: "label:urgent" };
void queryFilter;

// The MCP tool and its wrapper follow the SDK's reserved-word-safe spelling.
// @ts-expect-error `from` is the REST wire name, not the MCP input name.
const removedSenderFilter: ListMessagesParams = { from: "alice@example.com" };
void removedSenderFilter;

// Success payloads cross a second public boundary after the ergonomic SDK:
// MCP deliberately exposes REST-style snake_case, recursively. Keep the
// reserved-word-safe `from_` spelling uniform with the MCP input contract.
type NormalizedSuccess = McpOutput<{
  messageId: string;
  from_: string;
  deliveryMeta: { createdAt: string };
  attachments: Array<{ contentType: string; sizeBytes: number }>;
}>;

const normalizedSuccess: NormalizedSuccess = {
  message_id: "msg_1",
  from_: "alice@example.com",
  delivery_meta: { created_at: "2026-07-16T00:00:00Z" },
  attachments: [{ content_type: "text/plain", size_bytes: 1 }],
};
void normalizedSuccess;

// @ts-expect-error SDK camelCase must not be accepted as MCP output keys.
const leakedSdkSuccess: NormalizedSuccess = { messageId: "msg_1" };
void leakedSdkSuccess;
