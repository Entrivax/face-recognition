// Birthdate helpers for the person details modal.
//
// The edit form takes ONE text field using a dashed template where every
// component may be omitted (a `-` stands in the empty slot):
//
//	YYYY-MM-DD  full date          1965-03-02
//	YYYY--DD    year + day         1965--02   (month unknown)
//	YYYY-MM     year + month       1965-03
//	YYYY        year only          1965
//	-MM-DD      month + day        -03-02     (year unknown)
//	-MM-        month only         -03-
//
// Parsing is lenient on widths (1965-3-2 is accepted) and also accepts -MM
// without the trailing dash; the canonical serialized form is zero-padded as
// shown. All age/countdown date math is UTC-based and deterministic, with an
// injectable `now` for tests.

import type { BirthDate } from "./types";

/** The known components of a partial birthdate (absent = unknown). */
export interface BirthParts {
	year?: number;
	month?: number;
	day?: number;
}

export type BirthParse = { ok: true; parts: BirthParts } | { ok: false; error: string };

export const MONTHS = [
	"January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December",
];

// Component ranges (calendar-existence of complete dates is checked
// server-side; the field only enforces the format).
const MIN_YEAR = 1;
const MAX_YEAR = 2100;

/** Format error hint listing every accepted shape. */
export const BIRTH_FORMATS_HINT =
	"Formats: YYYY-MM-DD · YYYY--DD · YYYY-MM · YYYY · -MM-DD · -MM- (empty = unknown)";

// Ordered: the two-component patterns must not be shadowed by shorter ones.
const PATTERNS: { re: RegExp; pick: (m: RegExpMatchArray) => BirthParts }[] = [
	{ re: /^(\d{1,4})-(\d{1,2})-(\d{1,2})$/, pick: (m) => ({ year: +m[1], month: +m[2], day: +m[3] }) },
	{ re: /^(\d{1,4})--(\d{1,2})$/, pick: (m) => ({ year: +m[1], day: +m[2] }) },
	{ re: /^(\d{1,4})-(\d{1,2})$/, pick: (m) => ({ year: +m[1], month: +m[2] }) },
	{ re: /^(\d{1,4})$/, pick: (m) => ({ year: +m[1] }) },
	{ re: /^-(\d{1,2})-(\d{1,2})$/, pick: (m) => ({ month: +m[1], day: +m[2] }) },
	{ re: /^-(\d{1,2})-?$/, pick: (m) => ({ month: +m[1] }) },
];

/**
 * Parse the edit field's text. Empty/whitespace input parses to no parts
 * (clearing the birthdate); anything else must match one of the formats
 * above or the parse fails with a human-readable error.
 */
export function parseBirthText(src: string): BirthParse {
	const s = src.trim();
	if (!s) return { ok: true, parts: {} };
	for (const { re, pick } of PATTERNS) {
		const m = re.exec(s);
		if (!m) continue;
		const parts = pick(m);
		if (parts.year != null && (parts.year < MIN_YEAR || parts.year > MAX_YEAR)) {
			return { ok: false, error: `Year must be between ${MIN_YEAR} and ${MAX_YEAR}.` };
		}
		if (parts.month != null && (parts.month < 1 || parts.month > 12)) {
			return { ok: false, error: "Month must be between 1 and 12." };
		}
		if (parts.day != null && (parts.day < 1 || parts.day > 31)) {
			return { ok: false, error: "Day must be between 1 and 31." };
		}
		return { ok: true, parts };
	}
	return { ok: false, error: `Unrecognized birthdate. ${BIRTH_FORMATS_HINT}` };
}

const pad2 = (n: number) => String(n).padStart(2, "0");

/**
 * Serialize a stored birthdate back into the edit field's canonical text.
 * A day without a month and year is not expressible in the template and
 * yields the empty string.
 */
export function formatBirthText(b?: BirthDate | null): string {
	if (!b) return "";
	const y = b.year != null ? String(b.year) : "";
	const m = b.month != null ? pad2(b.month) : "";
	const d = b.day != null ? pad2(b.day) : "";
	if (y && m && d) return `${y}-${m}-${d}`;
	if (y && d) return `${y}--${d}`;
	if (y && m) return `${y}-${m}`;
	if (y) return y;
	if (m && d) return `-${m}-${d}`;
	if (m) return `-${m}-`;
	return "";
}

/**
 * The human text for the details view: "2 March 1965", "March 1965",
 * "2 March", "1965", "March" — and "1965 (day 2)" when the month is unknown.
 * Null when nothing is known.
 */
export function formatBirthTextual(b?: BirthDate | null): string | null {
	if (!b) return null;
	const { year, month, day } = b;
	const parts: string[] = [];
	if (day != null) parts.push(String(day));
	if (month != null) parts.push(MONTHS[month - 1] ?? `month ${month}`);
	if (year != null) parts.push(String(year));
	if (parts.length) return parts.join(" ");
	if (day != null) return `day ${day}`;
	return null;
}

/** Whole years elapsed between the (UTC) date and now. */
function yearsSince(y: number, m: number, d: number, now: Date): number {
	const ny = now.getUTCFullYear(), nm = now.getUTCMonth() + 1, nd = now.getUTCDate();
	let age = ny - y;
	if (nm < m || (nm === m && nd < d)) age--;
	return age;
}

/** 1-based UTC month/day of `now`. */
function utcMD(now: Date): [number, number] {
	return [now.getUTCMonth() + 1, now.getUTCDate()];
}

/**
 * Days from today until the next occurrence of (month, day) — 0 on the day
 * itself. A Feb 29 birthday falls back to Mar 1 in non-leap years.
 */
function daysToBirthday(month: number, day: number, now: Date): number {
	const [nm, nd] = utcMD(now);
	const ny = now.getUTCFullYear();
	const isLeap = (y: number) => (y % 4 === 0 && y % 100 !== 0) || y % 400 === 0;
	const target = (y: number): number => {
		// Feb 29 → Mar 1 when the target year isn't a leap year.
		const feb29 = month === 2 && day === 29 && !isLeap(y);
		const d = feb29 ? 1 : day;
		const m = feb29 ? 3 : month;
		return Math.round((Date.UTC(y, m - 1, d) - Date.UTC(ny, nm - 1, nd)) / 86_400_000);
	};
	// Try this year; a negative result (already passed) means next year's
	// occurrence.
	let days = target(ny);
	if (days < 0) days = target(ny + 1);
	return days;
}

/**
 * The age/countdown line for the details view, or null when nothing can be
 * computed: the exact age needs the full date; a partial one shows "≈" with
 * the birth date floored to the start of the month (day unknown) or year
 * (month unknown). The days-to-next-birthday part needs month and day.
 */
export function birthAgeLine(b?: BirthDate | null, now: Date = new Date()): string | null {
	if (!b) return null;
	const parts: string[] = [];

	if (b.year != null) {
		// Floor to the start of the month when the day is unknown, and to
		// the start of the year when the month is unknown too.
		const y = b.year;
		const m = b.month ?? 1;
		const d = b.month != null ? (b.day ?? 1) : 1;
		const age = yearsSince(y, m, d, now);
		const exact = b.month != null && b.day != null;
		parts.push(`${exact ? "" : "≈ "}${age} ${age === 1 ? "year" : "years"}`);
	}

	if (b.month != null && b.day != null) {
		const days = daysToBirthday(b.month, b.day, now);
		parts.push(days === 0 ? "birthday today" : `${days} ${days === 1 ? "day" : "days"} to their birthday`);
	}

	return parts.length ? parts.join(" · ") : null;
}
