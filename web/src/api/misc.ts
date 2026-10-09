// Health, config, recognition, and face-comparison API wrappers.

import type {
	CompareResponse,
	Config,
	Health,
	RecognizeResponse,
} from "../types";
import { expectJSON, parse } from "./core";

export async function getHealth(): Promise<Health> {
	const r = await fetch("/api/health");
	if (!r.ok) throw new Error("bad status");
	return parse<Health>(r);
}

export async function getConfig(): Promise<Config> {
	const r = await fetch("/api/config");
	return expectJSON(r, "could not read config");
}

// Persist the match threshold server-side (survives restarts).
export async function setThreshold(value: number): Promise<Config> {
	const r = await fetch("/api/config", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ threshold: value }),
	});
	return expectJSON(r, "could not set the threshold");
}

export async function recognize(file: File): Promise<RecognizeResponse> {
	const fd = new FormData();
	fd.append("image", file, file.name);
	const r = await fetch("/api/recognize", { method: "POST", body: fd });
	return expectJSON(r, "recognition failed");
}

// Face-to-face comparison of two arbitrary photos (admin): the server embeds
// the largest face of each and returns their cosine similarity. No identity
// database involved. 422 (no face in a photo) is thrown like any other error;
// the message names the offending photo.
export async function compareFaces(a: File, b: File): Promise<CompareResponse> {
	const fd = new FormData();
	fd.append("image1", a, a.name || "photo-a.jpg");
	fd.append("image2", b, b.name || "photo-b.jpg");
	const r = await fetch("/api/compare", { method: "POST", body: fd });
	return expectJSON(r, "comparison failed");
}
