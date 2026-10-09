// Small shared helpers.

export function fmtSize(bytes: number): string {
	return bytes >= 1024 * 1024
		? `${(bytes / (1024 * 1024)).toFixed(1)} MB`
		: `${Math.max(1, Math.round(bytes / 1024))} KB`;
}

export function initials(name: string): string {
	return name.trim().split(/\s+/).map((w) => w[0]).slice(0, 2).join("").toUpperCase();
}

// Image files currently on the clipboard, in item order.
export function imageFilesFromClipboard(dt: DataTransfer | null): File[] {
	if (!dt || !dt.items) return [];
	const out: File[] = [];
	for (const item of dt.items) {
		if (item.kind === "file" && item.type.startsWith("image/")) {
			const f = item.getAsFile();
			if (f) out.push(f);
		}
	}
	return out;
}

// "photo" / "photos" pluralizer helper used all over the UI.
export function plural(n: number, word = "photo"): string {
	return `${n} ${word}${n === 1 ? "" : "s"}`;
}

/**
 * Case- and diacritics-insensitive form of s for name comparisons: lowercase,
 * NFD-decompose, drop combining marks ("Émile" → "emile"). Letters that are
 * not accented forms of a base letter (Ø, Ł, Æ, ß — they have no
 * decomposition) keep their own place. Mirrors foldKey in internal/db/db.go,
 * which orders the server-side list the same way.
 */
export function foldText(s: string): string {
	return s.toLowerCase().normalize("NFD").replace(/\p{M}/gu, "");
}
