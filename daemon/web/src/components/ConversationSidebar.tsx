// SPDX-License-Identifier: Apache-2.0

import { MessageSquarePlus, Settings } from "lucide-react";
import { SystemsList } from "./SystemsList";

type Conversation = {
  id: string;
  title: string;
  created_at: string;
  updated_at: string;
};

export function ConversationSidebar({
  conversations,
  activeConversation,
  onSelect,
  onNew,
  onSettings,
  loading,
}: {
  conversations: Conversation[];
  activeConversation: string | null;
  onSelect: (id: string) => void;
  onNew: () => void;
  onSettings: () => void;
  loading?: boolean;
}) {
  function formatDate(dateStr: string) {
    if (!dateStr) return "Unknown";
    const date = new Date(dateStr);
    if (isNaN(date.getTime())) return "Unknown";
    const now = new Date();
    const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
    const msgDate = new Date(
      date.getFullYear(),
      date.getMonth(),
      date.getDate(),
    );

    if (msgDate.getTime() === today.getTime()) return "Today";
    if (msgDate.getTime() === today.getTime() - 86400000) return "Yesterday";
    return date.toLocaleDateString("en-US", {
      month: "short",
      day: "numeric",
    });
  }

  const grouped: Record<string, Conversation[]> = {};
  for (const conv of conversations) {
    const label = formatDate(conv.updated_at || conv.created_at);
    if (!grouped[label]) grouped[label] = [];
    grouped[label].push(conv);
  }

  return (
    <nav className="flex h-full flex-col" aria-label="Conversations">
      <div className="flex-1 overflow-y-auto p-3">
        {Object.entries(grouped).map(([date, convs]) => (
          <div key={date} className="mb-4">
            <div className="mb-1 px-2 text-[10px] font-medium uppercase tracking-widest text-slate-400">
              {date}
            </div>
            {convs.map((conv) => (
              <button
                key={conv.id}
                onClick={() => onSelect(conv.id)}
                aria-current={activeConversation === conv.id ? "true" : undefined}
                className={`mb-0.5 w-full rounded-lg px-2 py-1.5 text-left text-sm transition ${
                  activeConversation === conv.id
                    ? "bg-slate-950/5 text-slate-950"
                    : "text-slate-500 hover:bg-slate-950/[0.02] hover:text-slate-700"
                }`}
              >
                <span className="line-clamp-1">{conv.title || "New conversation"}</span>
              </button>
            ))}
          </div>
        ))}

        {loading && conversations.length === 0 && (
          <div className="animate-pulse space-y-3">
            <div className="mb-4">
              <div className="mb-2 h-2.5 w-12 rounded bg-slate-200/60" />
              <div className="space-y-1.5">
                <div className="h-7 w-full rounded-lg bg-slate-200/40" />
                <div className="h-7 w-3/4 rounded-lg bg-slate-200/40" />
              </div>
            </div>
            <div>
              <div className="mb-2 h-2.5 w-16 rounded bg-slate-200/60" />
              <div className="space-y-1.5">
                <div className="h-7 w-5/6 rounded-lg bg-slate-200/40" />
                <div className="h-7 w-2/3 rounded-lg bg-slate-200/40" />
              </div>
            </div>
          </div>
        )}

        {!loading && conversations.length === 0 && (
          <div className="px-2 py-8 text-center text-xs text-slate-400">
            No conversations yet. Send a message to get started.
          </div>
        )}
      </div>

      {/* Systems sidebar section. Self-renders only when at least one
          system exists, so a fresh machine keeps a clean sidebar. */}
      <SystemsList onOpenSettings={onSettings} />

      <div className="border-t border-slate-200/60 p-3">
        <button
          onClick={onNew}
          className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-sm text-slate-500 transition hover:bg-slate-950/[0.03] hover:text-slate-700"
        >
          <MessageSquarePlus className="h-4 w-4" aria-hidden="true" />
          <span>New conversation</span>
        </button>
        <button
          onClick={onSettings}
          className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-sm text-slate-500 transition hover:bg-slate-950/[0.03] hover:text-slate-700"
        >
          <Settings className="h-4 w-4" aria-hidden="true" />
          <span>Settings</span>
        </button>
      </div>
    </nav>
  );
}
