"use client";

// Left column of the dashboard inbox — scrollable list of threads with
// a sticky header. The list rows are deliberately compact (5 lines max)
// so the inbox can show ~12 threads in a 920px artboard.

import type { Thread } from "./threading";
import { ThreadRow } from "./ThreadRow";

export type ThreadListProps = {
  threads: Thread[];
  selectedKey: string | null;
  onSelect: (key: string) => void;
  /** Accepted for API compatibility; the count band was removed. */
  total?: number;
  pendingCount?: number;
  hasMore?: boolean;
  onLoadMore?: () => void;
  loadingMore?: boolean;
};

export function ThreadList({
  threads,
  selectedKey,
  onSelect,
  hasMore,
  onLoadMore,
  loadingMore,
}: ThreadListProps) {
  return (
    <div
      data-testid="thread-list"
      className="flex-1 flex flex-col"
      style={{
        background: "var(--bg-panel)",
      }}
    >
      {/* Rows — flow in the page's single scroll (no internal overflow
          container) so the inbox list scrolls with the document rather
          than nesting a second scrollbar. */}
      <div className="flex-1">
        {threads.length === 0 && (
          <div
            data-testid="thread-list-empty"
            className="px-4 py-8 text-center"
            style={{ color: "var(--fg-muted)", fontSize: 13 }}
          >
            No conversations yet.
          </div>
        )}
        {threads.map((t) => (
          <ThreadRow
            key={t.key}
            thread={t}
            active={t.key === selectedKey}
            onSelect={onSelect}
            historyIncomplete={hasMore}
          />
        ))}
        {hasMore && (
          <button
            type="button"
            onClick={onLoadMore}
            disabled={loadingMore}
            className="w-full text-center"
            style={{
              padding: "14px 16px 20px",
              fontFamily: "var(--f-mono)",
              fontSize: 11,
              color: loadingMore
                ? "var(--fg-subtle)"
                : "var(--accent-strong)",
              background: "transparent",
              border: "none",
              cursor: loadingMore ? "default" : "pointer",
            }}
          >
            {loadingMore ? "loading older…" : "load older →"}
          </button>
        )}
      </div>
    </div>
  );
}
