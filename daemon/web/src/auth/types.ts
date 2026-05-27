// SPDX-License-Identifier: Apache-2.0

export type WhoAmI = {
  sub: string;
  access: "control" | "view";
  name: string;
  email: string;
  expires_at: string;
};

export const PLATFORM_BASE = "https://www.vibecraft.so";

export function machineSlug(): string {
  return window.location.host.split(".")[0];
}

export function platformGrantURL(returnPath: string): string {
  const u = new URL(PLATFORM_BASE + "/auth/grant");
  u.searchParams.set("machine", machineSlug());
  u.searchParams.set("return", returnPath || "/");
  return u.toString();
}
