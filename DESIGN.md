---
name: ccswap
description: The daemon's local dashboard, drawn as a duty rota for a user's own Claude accounts.
colors:
  board: "#f4f5f2"
  surface: "#ffffff"
  raise: "#ffffff"
  ink: "#1d2a3a"
  muted-ink: "#5a6675"
  line: "#d9dee3"
  edge: "#c3cbd3"
  tape: "#d5e0eb"
  tape-major: "#a9bed3"
  track: "#e1e5e9"
  ahead-wash: "rgb(29 42 58 / .035)"
  highlight: "rgb(61 106 214 / .07)"
  highlight-solid: "#e7ebf0"
  critical: "#b42330"
  alert-ground: "#1d2a3a"
  alert-text: "#ffffff"
  alert-muted: "#b8c3cf"
  alert-code: "#34435a"
  alert-edge: "#5a6a7e"
  selection: "rgb(61 106 214 / .22)"
  skeleton: "#e5e8eb"
  board-dark: "#11161d"
  surface-dark: "#18202a"
  raise-dark: "#1e2833"
  ink-dark: "#e4e9ef"
  muted-ink-dark: "#9aa6b4"
  line-dark: "#253041"
  edge-dark: "#364457"
  tape-dark: "#1d2834"
  tape-major-dark: "#2e3e51"
  track-dark: "#273342"
  ahead-wash-dark: "rgb(228 233 239 / .03)"
  highlight-dark: "rgb(120 160 240 / .09)"
  highlight-solid-dark: "#1a2230"
  critical-dark: "#f0645a"
  alert-ground-dark: "#e4e9ef"
  alert-text-dark: "#11161d"
  alert-muted-dark: "#46525f"
  alert-code-dark: "#cdd5de"
  alert-edge-dark: "#8a96a4"
  selection-dark: "rgb(85 131 234 / .38)"
  skeleton-dark: "#1e2732"
  tag-0-signal-orange: "#d4621c"
  tag-1-ochre: "#b07c0a"
  tag-2-field-green: "#3f9a4a"
  tag-3-teal: "#1f8f84"
  tag-4-sky: "#2a8bc9"
  tag-5-cobalt: "#3d6ad6"
  tag-6-violet: "#7a5bd6"
  tag-7-magenta: "#b0479a"
  on-tag-dark: "#0b1118"
  on-tag-light: "#ffffff"
typography:
  display:
    fontFamily: "Geologica, system-ui, sans-serif"
    fontSize: "52px"
    fontWeight: 500
    lineHeight: 1
    letterSpacing: "-.04em"
    fontFeature: "\"tnum\""
  headline:
    fontFamily: "Geologica, system-ui, sans-serif"
    fontSize: "26px"
    fontWeight: 600
    lineHeight: 1.15
    letterSpacing: "-.02em"
  title:
    fontFamily: "Geologica, system-ui, sans-serif"
    fontSize: "20px"
    fontWeight: 600
  section:
    fontFamily: "Geologica, system-ui, sans-serif"
    fontSize: "16px"
    fontWeight: 600
    lineHeight: 1.45
  body:
    fontFamily: "Geologica, system-ui, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.45
    fontFeature: "\"tnum\""
  body-small:
    fontFamily: "Geologica, system-ui, sans-serif"
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.45
  label:
    fontFamily: "Geologica, system-ui, sans-serif"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: 1.45
  mono-scale:
    fontFamily: "\"JetBrains Mono\", ui-monospace, monospace"
    fontSize: "11px"
    fontWeight: 500
    lineHeight: "18px"
  mono-code:
    fontFamily: "\"JetBrains Mono\", ui-monospace, monospace"
    fontSize: ".92em"
    fontWeight: 500
    letterSpacing: "-.02em"
rounded:
  hair: "2px"
  tag: "3px"
  control: "4px"
  card: "6px"
  pill: "10px"
spacing:
  xxs: "4px"
  xs: "6px"
  sm: "8px"
  md: "12px"
  lg: "16px"
  xl: "20px"
  gutter: "32px"
  gutter-phone: "16px"
  control-height: "32px"
  control-height-coarse: "40px"
  lane-height: "34px"
  lane-label: "200px"
  lane-label-phone: "96px"
components:
  button:
    backgroundColor: "{colors.raise}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "0 12px"
    height: "{spacing.control-height}"
  button-hover:
    backgroundColor: "{colors.surface}"
  button-disabled:
    backgroundColor: "transparent"
    textColor: "{colors.muted-ink}"
  button-primary:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.board}"
    rounded: "{rounded.control}"
    padding: "0 12px"
    height: "{spacing.control-height}"
  button-primary-disabled:
    backgroundColor: "{colors.track}"
    textColor: "{colors.muted-ink}"
  button-quiet:
    backgroundColor: "transparent"
    textColor: "{colors.muted-ink}"
    rounded: "{rounded.control}"
    padding: "0 12px"
    height: "{spacing.control-height}"
  button-quiet-hover:
    backgroundColor: "{colors.track}"
    textColor: "{colors.ink}"
  select:
    backgroundColor: "{colors.raise}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "0 28px 0 10px"
    height: "{spacing.control-height}"
  input:
    backgroundColor: "{colors.raise}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "0 10px"
    height: "36px"
  switch-off:
    backgroundColor: "transparent"
    rounded: "{rounded.pill}"
    width: "36px"
    height: "20px"
  switch-on:
    backgroundColor: "{colors.ink}"
    rounded: "{rounded.pill}"
    width: "36px"
    height: "20px"
  stepper-step:
    backgroundColor: "{colors.raise}"
    rounded: "{rounded.control}"
    width: "28px"
    height: "28px"
  account-tag:
    backgroundColor: "{colors.tag-5-cobalt}"
    textColor: "{colors.on-tag-light}"
    typography: "{typography.headline}"
    rounded: "{rounded.tag}"
    padding: "0 16px"
    height: "44px"
  account-chip:
    backgroundColor: "{colors.tag-0-signal-orange}"
    rounded: "{rounded.hair}"
    width: "10px"
    height: "20px"
  duty-card:
    backgroundColor: "{colors.surface}"
    rounded: "{rounded.card}"
    padding: "16px 24px 10px"
  handover-panel:
    backgroundColor: "transparent"
    rounded: "{rounded.card}"
    padding: "14px 20px 12px"
  alert-strip:
    backgroundColor: "{colors.alert-ground}"
    textColor: "{colors.alert-text}"
    rounded: "{rounded.control}"
    padding: "7px 7px 7px 16px"
  message-strip:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.critical}"
    rounded: "{rounded.control}"
    padding: "10px 14px"
  rota-lane:
    height: "{spacing.lane-height}"
  table-row:
    height: "48px"
---

# Design System: ccswap

## Overview

**Creative North Star: "The Duty Rota"**

The dashboard is a shift board from a factory floor or a hospital ward: a white enamel board (a night board in dark mode), ink lettering, pale blue-grey grid tape, and one flat magnetic tag per account. Accounts work shifts. One is on duty, one is on deck, the rest rest until their limits reset, and the next handover sits on a real time axis. Every surface answers "who is in use, how much is left, what happens next" before anything else.

Density is operational, not decorative. Numbers are tabular and right-aligned where they compare; exact times beat relative phrases. The page is calm by default: ink, grey and the account tags carry it. It turns loud only when the user has to act, and then through a full-width ink alert strip or the single critical red, never through new colours.

The page refuses the category default of identical account cards with two progress bars each. Accounts appear as tags, lanes and table rows that share one state vocabulary, so a state learnt in one place reads the same everywhere.

**Key Characteristics:**
- One flat, full-strength tag colour per account, assigned deterministically from its name.
- One state vocabulary everywhere: solid in use, outline next, dashed projected, hatched resting, dashed grey needs login, greyed-out disabled.
- One rank device: a single soft shadow lifts the account in use; nothing else is lifted.
- Geologica for every word and number, with tabular figures; JetBrains Mono only for commands and the time scale.
- Light and dark follow the system; both boards are fully specified, not inverted.
- Admin and read-only are the same page; controls appear only where they work.

## Colors

A neutral enamel-and-ink board with eight flat account tags and one critical red; the neutrals carry the page, the tags identify, the red warns.

Every neutral exists twice, once per board (`*-dark` tokens in the frontmatter). The dark board is its own palette, not an inversion: surfaces step up in lightness (board, surface, raise) where the light board keeps surface and raise the same white.

### Primary
- **Ink** (`ink`): all text, the fill of usage bars and threshold ticks, the primary button, the checked switch, focus rings, the now line and flag on the rota, legend swatches. It is the page's only "accent" for controls; there is no brand blue for buttons.

### Account tags
The eight tag colours (`tag-0` to `tag-7`) are identity, not status. Each is the same value on both boards, and each holds at least 3:1 against either board. Text on a tag uses `on-tag-dark` for tags 0 to 4 and `on-tag-light` for tags 5 to 7.

- **Signal Orange, Ochre, Field Green, Teal, Sky, Cobalt, Violet, Magenta.** The set deliberately excludes red so an account can never be mistaken for the critical state.
- **Assignment:** accounts are visited in name order; each takes the colour its name hashes to (32-bit FNV-1a, modulo 8), or, if that is taken, the candidate that is not on a priority neighbour, then least used, then nearest to its hash. The first eight accounts therefore all differ; past eight a colour repeats, but not on an adjacent row while another is free. The result is the same on every machine for the same accounts in the same priority order: past eight the neighbour rule depends on that order, and an added account whose name sorts earlier can take a colour another account had, at any count.
- **The mark** uses Cobalt behind Signal Orange (`tag-5`, `tag-0`): two offset tags, the second stroked in the board colour. The favicon cannot read the page's tokens, so it repeats them in hex on the light board.

### Critical
- **Critical Red** (`critical`, `critical-dark`): a usage window over its threshold (number, bar fill), field errors, error strips, the daemon-down status dot. It never decorates.

### Neutral
- **Board** (`board`): the page ground. Also the knock-out colour under rota labels, projections and outlines so the grid tape does not run through them.
- **Surface / Raise** (`surface`, `raise`): the duty card, the login card, message strips (surface); buttons, selects and inputs (raise).
- **Muted Ink** (`muted-ink`): secondary text, labels, units, reset times, the off-state switch, the login dash.
- **Line** (`line`): row dividers and in-card rules. **Edge** (`edge`): control borders, panel borders, table head rule, disabled swatch, scrollbar thumb. **Track** (`track`): empty bar tracks, inline code ground, the quiet-button hover.
- **Grid Tape** (`tape`, `tape-major`): rota lane rules and hour lines; `tape-major` marks every sixth hour (00, 06, 12, 18), the track's right edge and the hover border of controls.
- **Ahead Wash** (`ahead-wash`): the faint shade over the future half of the rota.
- **Highlight** (`highlight`, `highlight-solid`): the linked hover of a rota lane and its table row; the solid variant sits behind labels that must cover what is under them.
- **Alert** (`alert-ground`, `alert-text`, `alert-muted`, `alert-code`, `alert-edge`): the inverted strip. On light it is ink with white text; on dark it is the pale ink with board text.
- **Selection** (`selection`) and **Skeleton** (`skeleton`): text selection and the loading placeholders.

### Named Rules
**The Flat Tag Rule.** An account colour is used at full strength as a solid fill, an outline, a dash or a hatch, never as a tint, gradient or transparent wash. There are two exceptions. Disabled is greyscale, not a lighter tag. The next account's outline fades out over its last 48px, because its end is not known; the fade ends the outline and never tints a fill.

**The No Red Tag Rule.** Red belongs to the critical state alone. A new tag colour must stay off the red hue, keep 3:1 on both boards, and ship with its on-tag text colour.

**The Ink Controls Rule.** Controls are ink, edge and raise. Tag colours never fill a button, a switch or a link.

## Typography

**Display Font:** Geologica (with system-ui, sans-serif), self-hosted variable woff2, weights 300 to 700
**Label/Mono Font:** JetBrains Mono (with ui-monospace, monospace), self-hosted woff2, weight 500

**Character:** Geologica is a geometric grotesque with full Cyrillic coverage that stays legible from 11 px labels to 52 px figures; JetBrains Mono is the board's stencil, used only where the content is a command or a clock.

The whole page sets `font-variant-numeric: tabular-nums`, so percentages, times and priorities align in columns.

### Hierarchy
- **Display** (500, 52px, line-height 1, -.04em): the 5-hour and weekly percentages of the account in use; the `%` sits at 22px/400 in muted ink. 40px on phones.
- **Headline** (600, 26px, 1.15, -.02em): the name inside the large account tag. 22px on phones.
- **Title** (600, 20px): the login heading, the empty-state heading, the "nobody in use" heading, the handover pair (from → to; 18px on phones). The wordmark is 20px/700/-.03em.
- **Section** (600, 16px): section headings (rota, accounts, rules). Panel headings (Next handover, How auto-switch decides) are 14px/600.
- **Body** (400, 14px, 1.45): everything else. Medium (500) for control labels, buttons, values in definition lists; 600 for `b`/`strong`.
- **Body small** (13px): notes, the handover policy sentence, per-model usage, row strips. 12.5px for hints, field errors, the email beside a name.
- **Label** (12px, muted): table heads, threshold captions, the legend, rota time labels.
- **Mono scale** (500, 11px, 18px line): rota hour labels and the now and handover flags.
- **Mono code** (.92em, -.02em): commands and paths inside running text, on a track-coloured ground with a 3px radius.

### Named Rules
**The One Family Rule.** Every word and number is Geologica. Mono appears only for commands and the time scale; never use it to make data look technical.

**The Tabular Rule.** Numbers that can change or be compared stay tabular. Reset times are exact clock times, right-aligned where they sit beside a figure. A time off today carries its weekday, and one six days or more away its date instead, since a weekday alone could mean another week.

## Layout

The page is a single column (`max-width` 1560px, centred) with a 32px gutter (16px on phones), 16px top and 56px bottom padding. Sections stack with 20px between them (24px on phones); a section's heading row sits 8px above its content. Spacing steps are 4, 6, 8, 12, 16, 20, 24 and 32px.

**Top bar:** a full-width bar (min-height 56px, line rule below) holding brand and host · version, the live status line, then the controls (auto-switch, strategy, check limits, language, log out) pushed right. At 1181px and wider it keeps one row: the status line's quiet clauses drop whole (Claude Code activity first, then the limits source) and "Check limits" folds to its icon before anything wraps. On phones the refresh button is always icon-only.

**Lead row:** a two-column grid, the duty card taking the remaining width and the handover panel fixed at 432px, 16px apart. At 1180px and below it stacks.

**Rota strip:** full width. A 200px label column (96px on phones) and a proportional track. 24 hours with five ahead on desktop; 9 hours with four ahead on phones. Lanes are 34px tall.

**Accounts table:** priority order, 48px rows, the account column takes all leftover width and ends in an ellipsis. Emails show beside names only when every row has room for the whole name plus the email's local part (or about twelve characters); otherwise every email hides and lives in the name's title. When columns do not fit, or at 1180px and below, the table becomes one labelled block per account (a four-column grid; two columns below 720px). A name left without room for about twelve characters once the emails hide counts as not fitting.

**Rules and policy:** a form grid (three columns; two at 1180px and below; one below 720px) beside a 400px explainer panel, 32px apart; the panel stacks under the form at 1180px and below.

**Breakpoints:** 1181px (one-row top bar), 1180px (lead, policy and skeleton stack, the table turns to blocks), 720px (phone). Coarse pointers raise the control height from 32px to 40px.

## Elevation & Depth

The board is flat. Depth is expressed by borders (edge for panels and controls, line for dividers), by dashed borders for absence, and by one shadow that marks rank, not layering.

### Shadow Vocabulary
- **Duty lift** (light `box-shadow: 0 1px 2px rgb(29 42 58 / .07), 0 12px 28px -16px rgb(29 42 58 / .4)`; dark `box-shadow: 0 0 0 1px rgb(255 255 255 / .05), 0 14px 32px -12px rgb(0 0 0 / .8)`): the duty card of the account in use. Nowhere else.

Two ring shadows exist but are knock-outs, not elevation: the rota puck (`0 0 0 2px` board, `0 0 0 3.5px` ink) and reset ticks (`0 0 0 1.5px` board) separate a mark from what is under it.

### Named Rules
**The One Rank Rule.** Only the account in use is lifted. When nobody is in use the duty card drops its shadow and becomes a dashed outline. Never add a shadow to a panel, a menu or a hover.

## Shapes

Corners are small and squared off, like enamel and magnetic tags: 2px for hairline marks (bars, segments, chips, swatches), 3px for tags and code, 4px for controls and strips, 6px for cards and panels, 10px only for the switch track. Round shapes are reserved for the switch thumb, status dot and spinner.

Line style carries meaning. Solid 1px edge: a panel or control. Dashed edge: something absent (no account in use, the empty state, the empty tag). Bars are 10px tall on the duty card and 6px in the table, with a threshold tick that overshoots the bar vertically.

## Components

### State vocabulary (shared by rota, table, legend and badges)
- **In use:** solid tag colour.
- **Next:** 2px outline in the tag colour over the board.
- **Projected:** 2px dashed tag colour, from now to the expected handover.
- **Resting until reset:** 1px tag-colour border with a -45° hatch of the tag colour (2px on 6px in lanes; 1.5px on 4px in small swatches).
- **Needs login:** dashed muted-ink border over the board; the state is grey because the account's colour is not the point, the user is.
- **Disabled:** the edge colour in swatches; in lanes and rows the tag goes greyscale at reduced opacity (.45 in lanes, .55 in rows) and the name turns muted.
- **Ready:** a track-coloured swatch.

The legend above the rota draws these in ink (needs login in muted ink, disabled in edge) so it reads independent of any account.

### Account tag and chip
- **Tag:** the large name plate on the duty card, solid tag colour, on-tag text, 3px radius, 44px min height (38px on phones). An empty dashed tag stands in when nobody is in use.
- **Chip:** a 10 × 20px vertical bar of the tag colour that precedes an account name in lanes, rows, alerts and the handover pair (8 × 16px in phone lanes).

### Duty card
- **Background:** surface, 6px radius, the duty lift, 16px 24px 10px padding.
- **Content:** tag, "in use since" and email, per-model weekly figures pushed right; then two gauges (5-hour wider than weekly, 40px apart): muted label, exact reset time right-aligned, display-size percentage, 10px bar with ink fill and an ink threshold tick, and the threshold caption under the tick. Over threshold, figure and fill turn critical. Unknown usage shows the figure muted without `%`.

### Handover panel
- 1px edge border, 6px radius, no fill. Heading and "projected" note; the from → to pair with chips and the projected time pushed right; pace and the policy sentence in 13px; a four-column threshold list (two on phones) above a line rule.

### Rota strip (signature)
- **Scale:** two 18px rows in mono. Flags: the now flag is an ink pill ending at the now line; the handover flag is a dashed box starting at the handover line, falling back to the bare time, then flipping to the left side, then hiding if it would collide with the now flag or the edge. Hour labels sit on a board knock-out so the vertical lines pass behind them; day changes print in ink.
- **Body:** lanes ruled with tape; hour lines in tape, every sixth hour in tape-major; the future half washed. The now line (2px ink) and handover line (1.5px dashed ink) run over the lanes and under the hour labels.
- **Segments:** past shifts and the current shift solid; projection dashed to the handover; the next account's outline starts there and fades out over its last 48px (or is an 18px stub at now when the time is unknown); resting lanes hatched to their reset; needs-login lanes boxed for the whole window with their words inside.
- **Puck:** an 8px tag-coloured bar at the now line, ringed in board and ink, sitting in the lane of the account in use. A switch slides it to its new lane (200ms, the standard ease).
- **Labels:** 12px (11.5px on phones) on a board knock-out (highlight-solid on a hovered lane). Each lane places its labels by rank (the account's own next change, then reset times, then words about now) and then by time. A label goes after its point, else before the segment it ends; in full, then without its muted detail, then in its short form. It may not cover another label (6px clearance), a segment (3px), the now or handover line (4px), or, on the first pass, a reset tick. A label that fits nowhere hides; the tick, segment and table still say it.
- **Reset ticks:** 2px tag-coloured marks with a board ring at each window reset inside the window.
- **Interaction:** one lane is a tab stop; arrow keys, Home and End move between lanes. A lane's accessible name gives its state, its next change and the shifts it held in the window. Hovering or focusing a lane highlights the same account's table row, and the reverse.

### Accounts table
- Header in 12px muted with an edge rule; rows 48px with line dividers. Columns: account (chip, name in 600, optional email), state (swatch from the state vocabulary, word, muted detail), 5-hour and weekly usage lines (right-aligned percentage, 6px mini bar with muted tick, muted reset), per-model (hidden when no account has one), next change, login and token (the login's end over the token's in muted, times alone with the words in their titles; the login's end in critical red within three days; a 28px icon button beside them that gets a new token, hidden for the account in use and one that needs a login, its icon spinning while it asks), priority stepper, auto switch, and the Use button (or a muted "in use"). A row's note or error strip sits beneath it, indented to the name.

### Buttons
- **Shape:** 4px radius, 32px tall (40px on coarse pointers), 0 12px padding, weight 500, a 4px invisible hit-area extension.
- **Default:** raise ground, edge border; hover moves the border to tape-major and the ground to surface; press scales to .96.
- **Primary:** ink ground with board text; hover drops to .9 opacity. One per form at most (log in, save rules).
- **Quiet:** no border or ground, muted text; hover adds a track ground and ink text. For log out and discard.
- **Busy:** a 13px ring spinner appears beside the label while the request runs.
- **Disabled:** muted text, transparent ground, line border, not-allowed cursor; while the daemon is offline the title says why.

### Inputs, selects, switches
- **Input:** 36px (40px on the login key field), raise ground, edge border, 4px radius; hover tape-major border; focus an ink border with the ring at zero offset; invalid a critical border and a 12.5px critical message below.
- **Select:** native, appearance reset, a muted chevron drawn inline (redrawn in the dark muted ink, as a data URI cannot read a token), 28px right padding.
- **Switch:** 36 × 20px pill. Off is a 1.5px muted outline with a muted thumb; on is a filled ink track with a board thumb that slides 16px (180ms). The state reads in either theme without colour.
- **Stepper:** 28px square −/+ buttons around a borderless 44px number that shows an edge border on hover.

### Strips and alerts
- **Alert strip:** full width, the inverted alert palette, 4px radius, min 44px. Shown only when the user must act (daemon unreachable, all exhausted, a login ccswap does not know, needs login, a login ending within three days), gravest first. Buttons inside are outlined in alert-edge; focus rings switch to alert text.
- **Message strip:** surface ground with a 1px border; critical (red border and text) for errors, warn (edge border, ink text) for advisories; the fault variant carries a Reload button and a report link.
- **Note:** 13px muted line for the read-only notice and host notes.

### Read-only and offline
- **Read-only:** the same page. Admin-only controls are removed from layout; values that were inputs render as plain 16px/500 values sized to their content; the top bar shows auto-switch and strategy as a muted sentence.
- **Offline:** the alert strip states the error and the time of the data on screen, announced as an alert once per change of text; the status dot turns critical; the rota stops at the data's time and its now flag says so; every control that asks the daemon is disabled with an explanatory title, and wakes when the daemon is back.
- **Loading:** skeleton blocks in the skeleton colour, 6px radius, appear only after 300ms.

### Motion
- **Ease:** `cubic-bezier(.23, 1, .32, 1)` for everything interactive.
- **Durations:** 120ms for hover highlights, 150ms for control colour and press, 180ms for the switch thumb, 200ms for the puck changing lanes, 250ms for bar fills. The boot mark's back tag shuffles 3–4px on a 1.6s alternating loop.
- **Reduced motion:** all transitions and animations collapse to zero; the puck jumps.

### Focus, selection, scrollbars
- **Focus:** 2px ink outline at 2px offset on every focusable element; inputs and the stepper use zero offset; lanes draw it inset. Inside the alert strip the ring is alert text.
- **Selection:** the selection token, text colour unchanged. Caret in ink.
- **Scrollbars:** thin, an edge-coloured thumb on a transparent track (WebKit: 10px with a 3px board border, 5px radius).

## Do's and Don'ts

### Do:
- **Do** draw any new account state with the existing vocabulary (solid, outline, dashed, hatched, dashed grey, greyscale) and add it to the legend, the table swatch and the rota together.
- **Do** take an account's colour from the shared assignment so the same account wears the same tag in every component.
- **Do** define every new neutral for both boards and check the tag set stays at 3:1 on each.
- **Do** keep figures tabular and times exact; put a muted unit or detail beside a number, not in its place.
- **Do** give every new text an i18n key and let layouts take Russian, which runs longer: wrap, ellipsis with a title, or drop whole clauses, never clip mid-word.
- **Do** hide controls that cannot work for the viewer instead of showing them disabled, and disable with a reason those that cannot work right now.
- **Do** honour reduced motion and keep every control keyboard-operable with the ink focus ring.

### Don't:
- **Don't** tint, fade, gradient or wash an account colour, or use one for a control; the next account's outline end is the one fade.
- **Don't** add a red or near-red tag colour; red means critical only.
- **Don't** add a second shadow; only the account in use is lifted.
- **Don't** use JetBrains Mono for anything but commands and the time scale, and don't add another family or a system display face.
- **Don't** let a rota label cover another label, a segment, a tick or the now and handover lines; hide it instead.
- **Don't** raise the alert strip for anything that does not need the user to act.
- **Don't** fall back to identical account cards with two progress bars each.
- **Don't** load fonts, images or scripts from anywhere but the daemon.
