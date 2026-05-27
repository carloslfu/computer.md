# VibeCraft brand & app-design baseline

> **This is the VibeCraft brand and app-design baseline shipped with the
> upstream computer.md OSS daemon.** Forks are welcome to replace it.
> The daemon reads from `/etc/vibecraft/brand.md` (preferred) or from
> the path in `BRAND_BASELINE_PATH` (env override). The embedded copy
> in the daemon binary is the fallback when neither is present.
>
> The contents below are design *discipline* — whitespace, hierarchy,
> restraint — not VibeCraft trademark or branding text. A fork wanting
> a different visual language ships a different file at the same path
> and the manager will read it.

This file is shipped to `/etc/vibecraft/brand.md` on every machine.
**The manager reads this when building any UI on the customer's behalf.**
Apps built without consulting it tend to look like a junior dev's first
stab. Apps that follow it look like part of the product.

Read this top to bottom before writing CSS. Skim it again before
declaring a build done.

---

## Tone

**Calm, commanding, precise.** Not enthusiastic, not playful, not
"AI-magical." The customer is a busy operator — they want
**control, relief, trust**. Quiet authority beats novelty.

Reference points for what "good" looks like: Stripe (clarity),
Linear (precision), Mercury (operator trust), Vercel (restraint).
Anti-reference: Notion (too soft), Superhuman (too power-user),
anything with gradient AI orbs / "magic" sparkle visuals.

If you'd ship a feature with a 🪄 emoji label, you're wrong.

## Layout

- **Generous whitespace.** Crowded UIs feel cheap. A page that looks
  half-empty in design is usually right.
- **One primary action per screen.** If two things look equally
  important, neither is.
- **Cards over borders.** Subtle elevation (`shadow-sm` or
  `shadow-md`), `rounded-xl`/`rounded-2xl`. Hard borders are loud.
- **Max content width.** `max-w-2xl` for forms / single-column,
  `max-w-5xl` for dashboards. Don't stretch to viewport width on
  desktop — it reads as unfinished.

## Color (light mode)

Light is the **default**. Switch only when the system reports dark.

| Use                          | Class / value                          |
|------------------------------|----------------------------------------|
| Page background              | `bg-[#f4f3ee]` (warm off-white)        |
| Surface (cards, modals)      | `bg-white` with `border-slate-200/60`  |
| Primary text                 | `text-slate-950`                       |
| Secondary text               | `text-slate-500`                       |
| Hairline / divider           | `border-slate-200/60`                  |
| Subtle hover                 | `hover:bg-slate-950/[0.03]`            |
| Active selection             | `bg-slate-950/5 text-slate-950`        |

Cool grays only. **No blue tint** in light mode. The off-white
background is the brand and shows everywhere.

## Color (dark mode)

Engage via `prefers-color-scheme: dark` and a manual toggle.
Don't default to dark unless the customer's OS says so.

| Use                          | Class / value                          |
|------------------------------|----------------------------------------|
| Page background              | `bg-slate-950` or `bg-[#0a0a0a]`       |
| Surface                      | `bg-slate-900` with `border-slate-800` |
| Primary text                 | `text-slate-50`                        |
| Secondary text               | `text-slate-400`                       |
| Subtle hover                 | `hover:bg-slate-50/[0.04]`             |

## Buttons

Primary is **pill-shaped** and **black-on-white** (or white-on-black
in dark). No gradients, no shadows on rest state.

```css
/* Primary */
rounded-full bg-slate-950 text-white text-sm font-medium px-5 py-2
hover:-translate-y-0.5 hover:shadow-lg hover:shadow-slate-950/20
transition

/* Secondary */
rounded-full bg-white text-slate-950 border border-slate-200
hover:bg-slate-50 transition

/* Destructive — used sparingly */
rounded-full bg-red-50 text-red-700 border border-red-200
hover:bg-red-100 transition
```

Never use bright-blue "primary" buttons. The black pill IS the brand.

## Type

- **Body:** Inter (`font-sans` after configuring), 14-16px, line-height
  1.5-1.65. Color `text-slate-500` for paragraphs, `text-slate-950`
  for emphasis.
- **Headings:** Poppins (or Inter if you're rushing), `font-semibold`,
  `tracking-tight`. Page title `text-3xl sm:text-4xl`.
- **Eyebrows / section labels:** `text-xs font-medium uppercase
  tracking-widest text-slate-400`.
- **No** ALL-CAPS body text. No light italics. No display fonts
  beyond the heading face.

## Density

Inputs / cards / list items: `py-3 px-4` minimum. The form on
`expense-tracker` is a good reference: form fields breathe, labels
are clear, the Add button is unmistakable.

## Copy

Same calm voice as the rest of the product:

- Sentence case for headings ("Add an expense", not "Add An Expense")
- Direct verbs, no hype. *"Save"* not *"Magically save ✨"*.
- No em dashes (use periods, commas, or colons)
- No AI-signal words (delve, seamless, harness, leverage, robust,
  comprehensive, innovative, unprecedented, streamline, furthermore)
- Acknowledge errors plainly. *"Couldn't connect. Retry?"* not
  *"Oops! Something went wrong 😬"*

## Light/dark toggle

Default to **light**. Follow `prefers-color-scheme` on first load.
Persist the user's manual override in localStorage so it survives
reloads. Don't show a system-detect indicator — just respect the
choice and let them change it.

A minimal toggle pattern:

```tsx
const [theme, setTheme] = useState<'light' | 'dark' | 'system'>('system');
// derive resolved theme via prefers-color-scheme, apply via
// document.documentElement.classList.toggle('dark', isDark).
```

Or use `next-themes` (preinstalled in the scaffold) which handles
this with no boilerplate.

## What "high quality" looks like in 5 seconds

When you open the URL after building:

1. Page loads under 1s
2. The first thing visible is the primary action the customer asked
   for, not navigation
3. Spacing and font sizes look like the rest of the dashboard, not
   a different product
4. Light mode by default; toggling to dark doesn't break anything
5. No console errors, no jagged-aliased icons, no horizontal scroll

If any of those five fail, the app isn't done — go back and fix.

## What "low quality" looks like (avoid)

- Default Bootstrap blue
- All-borders no whitespace
- Centered single-column max-width:1200px (looks like 2012)
- Big colorful "AI ✨" buttons
- White text on a vivid gradient background
- Tiny dense table with no row hover
- Generic "Welcome!" hero with a stock illustration

When you catch yourself building any of these, stop. Re-read the
top of this file.
