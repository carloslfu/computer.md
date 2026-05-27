// SPDX-License-Identifier: Apache-2.0

// Presence indicator for the chat header.
//
// Why this exists, not an exclusive-control lock: this product has team
// plans (Pro / Business / Enterprise) where multiple humans share one
// computer, AND a single human routinely opens the dashboard in multiple
// tabs (laptop + phone, two browser windows). A control-lock model fails
// the multi-tab case immediately ("Alice is using this computer" —
// against herself) and turns coordination into a hand-off ceremony in
// the team case.
//
// Instead: presence (the Google Docs / Linear / Figma pattern). Show
// who else is connected to the same machine, deduped by user. No locks,
// no banners, no idle-steal. If two teammates collide, the symptoms are
// loud in the chat and they coordinate as humans.
//
// We hide the row entirely when no one else is here — it should be
// invisible chrome in the single-user case.

export type PresenceUser = {
  user_id: string;
  name?: string;
  email?: string;
};

// 8 muted, hue-spread colors that read well on the warm off-white surface.
// Picked from the same palette family as the rest of the UI (slate/emerald
// neutrals, not the brand accent). Stable mapping userId → color so the
// same person looks the same across sessions.
const AVATAR_COLORS = [
  "bg-emerald-500",
  "bg-sky-500",
  "bg-violet-500",
  "bg-amber-500",
  "bg-rose-500",
  "bg-teal-500",
  "bg-indigo-500",
  "bg-orange-500",
];

function colorForUser(userId: string): string {
  let h = 0;
  for (let i = 0; i < userId.length; i++) {
    h = (h * 31 + userId.charCodeAt(i)) | 0;
  }
  return AVATAR_COLORS[Math.abs(h) % AVATAR_COLORS.length];
}

function initialsFor(user: PresenceUser): string {
  const source = user.name || user.email || user.user_id;
  if (!source) return "?";
  // Prefer first letters of first + last word of name. Falls back to
  // first letter of the source string.
  const words = source.trim().split(/\s+/);
  if (words.length >= 2) {
    return (words[0][0] + words[words.length - 1][0]).toUpperCase();
  }
  return source[0].toUpperCase();
}

function labelFor(user: PresenceUser): string {
  if (user.name && user.email) return `${user.name} · ${user.email}`;
  return user.name || user.email || user.user_id;
}

export function PresenceAvatars({
  users,
  selfUserId,
}: {
  users: PresenceUser[];
  selfUserId: string;
}) {
  // Only show OTHER users. Showing yourself in the chat header is noise
  // in the single-user case and tells you nothing in the multi-user
  // case (you already know you're here).
  const others = users.filter(
    (u) => u.user_id && u.user_id !== selfUserId,
  );
  if (others.length === 0) return null;

  // Cap visible avatars at 4; collapse the rest into a "+N" tile.
  const visible = others.slice(0, 4);
  const overflow = others.length - visible.length;

  return (
    <div
      className="flex -space-x-1.5"
      role="group"
      aria-label={`${others.length} other ${others.length === 1 ? "person" : "people"} here`}
    >
      {visible.map((u) => (
        <div
          key={u.user_id}
          title={labelFor(u)}
          className={`flex h-6 w-6 items-center justify-center rounded-full text-[10px] font-semibold text-white ring-2 ring-[#f4f3ee] ${colorForUser(u.user_id)}`}
        >
          {initialsFor(u)}
        </div>
      ))}
      {overflow > 0 && (
        <div
          title={`${overflow} more`}
          className="flex h-6 w-6 items-center justify-center rounded-full bg-slate-300 text-[10px] font-semibold text-slate-700 ring-2 ring-[#f4f3ee]"
        >
          +{overflow}
        </div>
      )}
    </div>
  );
}
