// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from "react";
import { Bell } from "lucide-react";
import { PLATFORM_BASE } from "../auth/types";

type Notification = {
  id: string;
  machineId: string;
  kind: string;
  title: string;
  body: string | null;
  conversationId: string | null;
  priority: "low" | "normal" | "high";
  readAt: string | null;
  createdAt: string;
};

// Cross-origin bell. The platform owns the notifications table — daemons
// forward into it via /notify. From the per-machine SPA we fetch
// platform's /api/notifications with credentials:'include'. The
// platform serves matching CORS headers (see lib/cors.ts) so the
// response is readable across origins.
export function NotificationBell() {
  const [items, setItems] = useState<Notification[]>([]);
  const [unread, setUnread] = useState(0);
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  const fetchItems = useCallback(async () => {
    try {
      // eslint-disable-next-line no-restricted-syntax -- cross-origin to www.vibecraft.so; apiFetch is same-origin-daemon only
      const res = await fetch(PLATFORM_BASE + "/api/notifications", {
        credentials: "include",
        headers: { Accept: "application/json" },
      });
      if (!res.ok) return;
      const data = (await res.json()) as {
        items?: Notification[];
        unreadCount?: number;
      };
      setItems(data.items || []);
      setUnread(data.unreadCount || 0);
    } catch {
      // Best-effort polling; offline/error simply leaves the existing
      // list in place.
    }
  }, []);

  useEffect(() => {
    fetchItems();
    const id = setInterval(fetchItems, 30000);
    const onFocus = () => fetchItems();
    window.addEventListener("focus", onFocus);
    return () => {
      clearInterval(id);
      window.removeEventListener("focus", onFocus);
    };
  }, [fetchItems]);

  useEffect(() => {
    if (!open) return;
    const handler = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open]);

  // Mark visible items as read when the dropdown opens.
  useEffect(() => {
    if (!open) return;
    const unreadItems = items.filter((it) => !it.readAt);
    if (unreadItems.length === 0) return;
    Promise.all(
      unreadItems.map((it) =>
        // eslint-disable-next-line no-restricted-syntax -- cross-origin to www.vibecraft.so; apiFetch is same-origin-daemon only
        fetch(`${PLATFORM_BASE}/api/notifications/${it.id}/read`, {
          method: "POST",
          credentials: "include",
        }).catch(() => null),
      ),
    ).then(() => {
      setItems((prev) =>
        prev.map((it) =>
          it.readAt ? it : { ...it, readAt: new Date().toISOString() },
        ),
      );
      setUnread(0);
    });
  }, [open, items]);

  return (
    <div className="relative" ref={rootRef}>
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-label={
          unread > 0 ? `Notifications (${unread} unread)` : "Notifications"
        }
        className="relative flex h-9 w-9 items-center justify-center rounded-full text-slate-400 transition hover:bg-slate-100 hover:text-slate-700"
      >
        <Bell className="h-4 w-4" aria-hidden="true" />
        {unread > 0 && (
          <span
            className="absolute right-1.5 top-1.5 flex h-4 min-w-[1rem] items-center justify-center rounded-full bg-slate-950 px-1 text-[10px] font-medium text-white"
            aria-hidden="true"
          >
            {unread > 9 ? "9+" : unread}
          </span>
        )}
      </button>

      {open && (
        <div className="absolute right-0 top-11 z-40 w-80 max-w-[calc(100vw-2rem)] overflow-hidden rounded-xl border border-slate-200/60 bg-white shadow-lg">
          <div className="border-b border-slate-100 px-4 py-3">
            <p className="text-xs font-medium uppercase tracking-widest text-slate-400">
              From your computer
            </p>
          </div>
          {items.length === 0 ? (
            <p className="px-4 py-6 text-sm text-slate-500">
              No notifications yet. The manager will write here when systems
              have something to report.
            </p>
          ) : (
            <ul className="max-h-96 overflow-y-auto divide-y divide-slate-100">
              {items.map((it) => (
                <li key={it.id} className="hover:bg-slate-50">
                  <a
                    href={
                      it.conversationId
                        ? `${PLATFORM_BASE}/dashboard?machine=${it.machineId}&chat=${encodeURIComponent(it.conversationId)}`
                        : `${PLATFORM_BASE}/dashboard?machine=${it.machineId}`
                    }
                    className="block px-4 py-3"
                    onClick={() => setOpen(false)}
                  >
                    <div className="flex items-start justify-between gap-3">
                      <p
                        className={`text-sm leading-snug ${
                          it.readAt
                            ? "text-slate-600"
                            : "font-medium text-slate-950"
                        }`}
                      >
                        {it.title}
                      </p>
                      {!it.readAt && (
                        <span
                          className="mt-1.5 h-1.5 w-1.5 flex-shrink-0 rounded-full bg-slate-950"
                          aria-hidden="true"
                        />
                      )}
                    </div>
                    {it.body && (
                      <p className="mt-1 line-clamp-2 text-xs leading-relaxed text-slate-500">
                        {it.body}
                      </p>
                    )}
                    <p className="mt-1.5 text-[10px] uppercase tracking-widest text-slate-400">
                      {new Date(it.createdAt).toLocaleString(undefined, {
                        month: "short",
                        day: "numeric",
                        hour: "numeric",
                        minute: "2-digit",
                      })}
                    </p>
                  </a>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}
