// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { PresenceAvatars, type PresenceUser } from "./PresenceAvatars";

const alice: PresenceUser = {
  user_id: "u_alice",
  name: "Alice Anderson",
  email: "alice@example.com",
};
const bob: PresenceUser = {
  user_id: "u_bob",
  name: "Bob Brown",
  email: "bob@example.com",
};
const carol: PresenceUser = {
  user_id: "u_carol",
  name: "Carol Carter",
  email: "carol@example.com",
};
const dave: PresenceUser = {
  user_id: "u_dave",
  name: "Dave Davis",
  email: "dave@example.com",
};
const eve: PresenceUser = {
  user_id: "u_eve",
  name: "Eve Evans",
  email: "eve@example.com",
};
const frank: PresenceUser = {
  user_id: "u_frank",
  name: "Frank Foster",
  email: "frank@example.com",
};

describe("PresenceAvatars", () => {
  it("renders nothing when only self is present (single-user case)", () => {
    const { container } = render(
      <PresenceAvatars users={[alice]} selfUserId="u_alice" />,
    );
    // The component should render nothing — single-user chat header
    // must not have a presence row taking up space for no signal.
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing when no users are passed", () => {
    const { container } = render(
      <PresenceAvatars users={[]} selfUserId="u_alice" />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("renders an avatar for each other user", () => {
    render(<PresenceAvatars users={[alice, bob]} selfUserId="u_alice" />);
    // Bob's initials (BB) should be on screen; Alice should not.
    expect(screen.getByText("BB")).toBeTruthy();
    expect(screen.queryByText("AA")).toBeNull();
  });

  it("dedupes is handled upstream; component renders all passed users", () => {
    // The broker dedupes — same userId appears at most once in the
    // `users` prop. The component trusts that. This test pins the
    // contract: 2 different users in → 2 avatars out.
    render(<PresenceAvatars users={[bob, carol]} selfUserId="u_alice" />);
    expect(screen.getByText("BB")).toBeTruthy();
    expect(screen.getByText("CC")).toBeTruthy();
  });

  it("filters out self even if self appears in the list", () => {
    // The daemon may include self in the presence event (it does, by
    // design — symmetry, every client sees the same payload). The
    // component is the place that filters self out.
    render(
      <PresenceAvatars users={[alice, bob, carol]} selfUserId="u_alice" />,
    );
    expect(screen.queryByText("AA")).toBeNull();
    expect(screen.getByText("BB")).toBeTruthy();
    expect(screen.getByText("CC")).toBeTruthy();
  });

  it("collapses 5+ others into a +N overflow tile", () => {
    // 5 others + self → 4 visible avatars + a "+1" tile.
    render(
      <PresenceAvatars
        users={[alice, bob, carol, dave, eve, frank]}
        selfUserId="u_alice"
      />,
    );
    expect(screen.getByText("BB")).toBeTruthy();
    expect(screen.getByText("CC")).toBeTruthy();
    expect(screen.getByText("DD")).toBeTruthy();
    expect(screen.getByText("EE")).toBeTruthy();
    // Frank should be in the overflow, not rendered as an avatar.
    expect(screen.queryByText("FF")).toBeNull();
    expect(screen.getByText("+1")).toBeTruthy();
  });

  it("uses email when name is absent", () => {
    const anon: PresenceUser = { user_id: "u_anon", email: "a@b.io" };
    render(<PresenceAvatars users={[anon]} selfUserId="u_self" />);
    // Single-word source → first character.
    expect(screen.getByText("A")).toBeTruthy();
  });

  it("falls back to user_id when no name or email", () => {
    const minimal: PresenceUser = { user_id: "u_xyz" };
    render(<PresenceAvatars users={[minimal]} selfUserId="u_self" />);
    expect(screen.getByText("U")).toBeTruthy();
  });

  it("ignores entries with empty user_id (defensive)", () => {
    const { container } = render(
      <PresenceAvatars
        users={[alice, { user_id: "" }]}
        selfUserId="u_alice"
      />,
    );
    // Only Alice was non-self, and the empty entry should not render.
    // After filtering self (Alice) AND the empty entry, nothing is left.
    expect(container.firstChild).toBeNull();
  });

  it("provides accessible group label reflecting count", () => {
    const { container } = render(
      <PresenceAvatars users={[alice, bob, carol]} selfUserId="u_alice" />,
    );
    const group = container.querySelector('[role="group"]');
    expect(group?.getAttribute("aria-label")).toBe("2 other people here");
  });

  it("uses singular form for exactly one other", () => {
    const { container } = render(
      <PresenceAvatars users={[alice, bob]} selfUserId="u_alice" />,
    );
    const group = container.querySelector('[role="group"]');
    expect(group?.getAttribute("aria-label")).toBe("1 other person here");
  });

  it("title attribute carries name + email for hover", () => {
    const { container } = render(
      <PresenceAvatars users={[alice, bob]} selfUserId="u_alice" />,
    );
    const bobAvatar = container.querySelector('[title*="Bob Brown"]');
    expect(bobAvatar).toBeTruthy();
    expect(bobAvatar?.getAttribute("title")).toContain("bob@example.com");
  });
});
