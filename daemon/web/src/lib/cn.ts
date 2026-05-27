// SPDX-License-Identifier: Apache-2.0

import clsx from "clsx";

// Tiny class joiner. We don't pull in tailwind-merge here — the SPA's
// component set is small enough that callers stay disciplined about
// class collisions. Add tailwind-merge later if it bites.
export const cn = clsx;
