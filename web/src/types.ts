// API data-transfer types, mirroring the JSON shapes internal/api/api.go emits.

export interface Health {
	status: string;
	people: number;
	threshold: number;
}

export interface Config {
	threshold: number;
}

export interface PersonSummary {
	id: string;
	name: string;
	photos: number;
	embeddings: number;
	thumb: string;
}

export interface PeopleResponse {
	people: PersonSummary[];
	count: number;
}

export interface PersonPhoto {
	path: string;
	hash: string;
}

export interface PersonDetail {
	id: string;
	name: string;
	thumb_src: string;
	photos: PersonPhoto[];
}

export interface Match {
	person_id: string;
	name: string;
	score: number;
}

export interface Face {
	bbox: [number, number, number, number]; // x, y, width, height
	score: number; // detector confidence 0..1
	landmarks: [number, number][]; // 5-point landmarks
	name: string; // matched identity, or "unknown"
	person_id: string; // matched person's ID, or ""
	confidence: number; // best cosine similarity 0..1
	matches?: Match[]; // every identity above the threshold, ranked best-first
}

export interface RecognizeResponse {
	count: number;
	faces: Face[];
}

export interface DetectResponse {
	photo: string;
	count: number;
	faces: Face[];
}

export interface EnrollResponse {
	person: string;
	added: number;
	total: number;
	saved: string[];
	failures?: string[];
}

export interface EnrollFaceResponse {
	person: string;
	saved: string;
}

export interface DeletePersonResponse {
	removed: string;
	folder_removed: boolean;
}

export interface RenameResponse {
	renamed: boolean;
	old_name: string;
	name: string;
	id: string;
}

export interface DeletePhotoResponse {
	removed: string;
	photos: number;
}

export interface ThumbResponse {
	person: string;
	thumb: string;
}

export interface RescanResponse {
	people: number;
	added: number;
	kept: number;
	failed: number;
	pruned: number;
	skipped_count: number;
}

// A photo opened in the enlarged face-check viewer. Faces may arrive after
// the viewer opens (the enroll modal's check chain reports them later).
export interface CheckView {
	src: string;
	title: string;
	faces: Face[] | null;
}
