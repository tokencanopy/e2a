# Domain Context

This glossary records product terms whose distinctions are important across
e2a's API, storage, and UI. It is intentionally about domain meaning rather
than implementation structure.

## Agent mailbox

The messages visible to one e2a agent identity. Email thread identity is local
to this boundary. Two agent mailboxes receiving the same physical email do not
share an e2a thread identity.

## Application conversation

Application-level workflow correlation represented by `conversation_id`. A
caller may choose the value; e2a may mint or inherit a fallback when it is
omitted. An application conversation may be a backend-agent session, run,
ticket, task, or other unit chosen by the API caller. It is not necessarily an
email thread.

## Email thread

A mailbox-local connected reply exchange. A fresh send, inbound root, or
forward starts a thread; a reply joins its resolved parent's thread. e2a
materializes this identity as server-owned `thread_id`.

## RFC reply topology

The on-wire relationships expressed by `Message-ID`, `In-Reply-To`, and
`References`. Mail clients use these fields, sometimes together with their own
heuristics, to display conversations. e2a uses this topology as evidence when
assigning inbound messages to email threads.

## Reply parent

The earlier message directly referenced by a reply. An API reply's referenced
message resource is authoritative inside e2a. An inbound SMTP reply parent is
resolved from RFC reply headers. A physical delivery twin is not a reply
parent.

## Thread identity

The opaque, e2a-owned, mailbox-local `thread_id` assigned to messages in one
email thread. Callers cannot set it, and e2a does not transport it in SMTP
headers.

## Delivery twin

Two e2a message records representing one physical email in the same mailbox,
such as Sent and Inbox records for a self-send or a platform test message that
returns through SMTP. Twins share a thread but do not have a reply-parent
relationship.

## Legacy threadless message

A message created before server-owned thread assignment was enabled, or during
a rollback window, whose `thread_id` remains null. e2a does not bulk-backfill
these rows. A new message may lazily assign one exact old parent while replying
to it, but does not reconstruct that parent's earlier conversation graph.
