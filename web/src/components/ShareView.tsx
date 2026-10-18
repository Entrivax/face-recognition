// Share links — /share/<repoID>/<token>.
//
// The Go server serves index.html for that path shape, so the SPA boots
// straight into ShareView (no history.pushState involved anywhere). Share
// mode authenticates with "X-Share-Token: <repoID>/<token>" on every API
// call (injected by api/core via setShareContext) and never sends the
// browser's cookies, so a share visitor cannot touch a signed-in session.
// Only reader routes may be used; 401/403 flip the view to an error card
// ("this share link is no longer valid").
//
// parseSharePath is exported for App: it classifies the boot pathname.

import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import * as api from "../api";
import { ShareInvalidError, setOnShareInvalid, setShareContext } from "../api/core";
import type { Health, PersonDetail, PersonSummary } from "../types";
import { handleEscape } from "../modalStack";
import { imageFilesFromClipboard } from "../util";
import { Stage } from "./Stage";
import { PeoplePanel, type Perms } from "./PeoplePanel";
import { PersonModal } from "./PersonModal";

export type SharePath =
	| { kind: "share"; repoId: string; token: string }
	| { kind: "malformed" }
	| { kind: "none" };

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const TOKEN_RE = /^[A-Za-z0-9_-]{1,128}$/;

/** Classify the boot pathname: a share view, a malformed share intent, or no share. */
export function parseSharePath(pathname: string): SharePath {
	const parts = pathname.split("/").filter(Boolean);
	if (parts[0] !== "share") return { kind: "none" };
	if (parts.length !== 3) return { kind: "malformed" };
	const [, rawRepoId, token] = parts; // [0] is the literal "share" segment
	const repoId = rawRepoId.toLowerCase(); // IDs are lowercase; UUID_RE folds case anyway
	if (!UUID_RE.test(repoId) || !TOKEN_RE.test(token)) return { kind: "malformed" };
	return { kind: "share", repoId, token };
}

/** Short display form of a repo id (full id in the title). */
function shortRepoId(id: string): string {
	return id.length > 12 ? `${id.slice(0, 8)}…` : id;
}

interface ShareViewProps {
	repoId: string;
	token: string;
}

type SharePhase = "loading" | "ready" | "invalid";

export function ShareView({ repoId, token }: ShareViewProps) {
	const [phase, setPhase] = useState<SharePhase>("loading");
	const [repoName, setRepoName] = useState<string | null>(null);
	const [people, setPeople] = useState<PersonSummary[]>([]);
	const [peopleErr, setPeopleErr] = useState(false);
	const [health, setHealth] = useState<Health | null>(null);
	const [healthErr, setHealthErr] = useState(false);
	const [detailsName, setDetailsName] = useState<string | null>(null);

	const inspectRef = useRef<((file: File) => void) | null>(null);
	const registerInspect = useCallback((fn: ((file: File) => void) | null) => {
		inspectRef.current = fn;
	}, []);

	// Attach the share context to every API call and clear it again on
	// unmount (a visitor who then somehow re-enters the app must not keep
	// the header). 401/403 anywhere flip the phase to the error card.
	useEffect(() => {
		setShareContext({ repoId, token });
		setOnShareInvalid(() => setPhase("invalid"));
		return () => {
			setShareContext(null);
			setOnShareInvalid(null);
		};
	}, [repoId, token]);

	const checkHealth = useCallback(async () => {
		try {
			const h = await api.getHealth();
			setHealth(h);
			setHealthErr(false);
		} catch {
			setHealthErr(true);
		}
	}, []);

	const loadPeople = useCallback(async () => {
		try {
			const j = await api.getPeople(repoId);
			setPeople(j.people || []);
			setPeopleErr(false);
			setPhase("ready");
		} catch (e) {
			if (e instanceof ShareInvalidError) return; // the hook already flipped the phase
			setPeopleErr(true);
			setPhase("ready");
		}
	}, [repoId]);

	// Boot: verify the token by loading the repo's people list.
	useEffect(() => {
		void loadPeople();
	}, [loadPeople]);

	useEffect(() => {
		checkHealth();
		const t = window.setInterval(checkHealth, 30000);
		return () => window.clearInterval(t);
	}, [checkHealth]);

	// Escape closes the topmost modal.
	useEffect(() => {
		const onKey = (e: KeyboardEvent) => {
			if (e.key === "Escape") handleEscape();
		};
		document.addEventListener("keydown", onKey);
		return () => document.removeEventListener("keydown", onKey);
	}, []);

	// Paste inspects on the stage — while the details modal is closed.
	useEffect(() => {
		const onPaste = (e: ClipboardEvent) => {
			if (detailsName !== null) return; // a modal is open — don't hijack
			const files = imageFilesFromClipboard(e.clipboardData);
			if (!files.length) return;
			const t = e.target as HTMLElement | null;
			if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA") &&
					typeof e.clipboardData?.getData === "function" &&
					e.clipboardData.getData("text/plain")) {
				return;
			}
			e.preventDefault();
			inspectRef.current?.(files[0]);
		};
		document.addEventListener("paste", onPaste);
		return () => document.removeEventListener("paste", onPaste);
	}, [detailsName]);

	// Body scroll lock while the details modal is open.
	useEffect(() => {
		document.body.classList.toggle("modal-open", detailsName !== null);
	}, [detailsName]);

	// A person detail carries repo_name — the one place a share visitor can
	// learn the repository's human name (the repos list needs a session).
	const onDetailLoaded = useCallback((d: PersonDetail) => {
		if (d.repo_name) setRepoName(d.repo_name);
	}, []);

	if (phase === "loading") {
		return (
			<div class="auth-page">
				<div class="auth-card">
					<p class="auth-lede" aria-live="polite">Loading shared repository…</p>
				</div>
			</div>
		);
	}

	if (phase === "invalid") {
		return (
			<div class="auth-page">
				<div class="auth-card">
					<p class="auth-eyebrow">share link</p>
					<h1>This share link is no longer valid</h1>
					<p class="auth-lede">
						The link may have been revoked by the administrator, or the repository
						it pointed at is gone. Ask the owner for a fresh link.
					</p>
					<a class="btn" href="/">Go to the console</a>
				</div>
			</div>
		);
	}

	const perms: Perms = { user: false, writer: false, admin: false };
	const cls = healthErr ? "status err" : health ? "status ok" : "status";
	const statusText = healthErr ? "server unreachable" : health ? health.status : "connecting…";

	return (
		<>
			<header class="topbar share-topbar">
				<div class="brand">
					<span class="brand-mark" aria-hidden="true">
						<img src="/logo.svg" />
					</span>
					<span class="brand-word">recogn</span>
				</div>
				<div class="share-banner">
					<span class="share-banner-label">read-only shared repository</span>
					<span class="share-banner-name" title={repoId}>{repoName ?? shortRepoId(repoId)}</span>
				</div>
				<div class={cls} id="status">
					<span class="status-dot" id="statusDot" />
					<span id="statusText">{statusText}</span>
				</div>
			</header>

			<main class="layout">
				<Stage
					canEnroll={false}
					repoId={repoId}
					onEnrolled={() => { void loadPeople(); }}
					registerInspect={registerInspect}
					onEditOpenChange={() => { /* the editor is writer-only — never opens here */ }}
				/>
				<PeoplePanel
					perms={perms}
					people={people}
					peopleErr={peopleErr}
					threshold={null}
					onThresholdCommit={() => { /* readers cannot change the threshold */ }}
					onRescan={async () => { /* share visitors cannot rescan */ }}
					onOpenPerson={setDetailsName}
					onRemove={() => { /* share visitors cannot remove people */ }}
					onEnrollClick={() => { /* share visitors cannot enroll */ }}
					onCompareClick={() => { /* compare needs a session */ }}
				/>
			</main>

			<PersonModal
				person={detailsName}
				repoId={repoId}
				canManage={false}
				thumb={detailsName ? people.find((p) => p.name === detailsName)?.thumb ?? null : null}
				onCloseRequest={() => setDetailsName(null)}
				onManagePhotos={() => { /* share visitors cannot manage photos */ }}
				onChange={() => { void loadPeople(); }}
				onDetailLoaded={onDetailLoaded}
			/>
		</>
	);
}

/** Malformed share intent (e.g. /share/123 without a token): error card. */
export function ShareMalformed() {
	return (
		<div class="auth-page">
			<div class="auth-card">
				<p class="auth-eyebrow">share link</p>
				<h1>This share link is malformed</h1>
				<p class="auth-lede">
					Share links look like <code>/share/&lt;repository&gt;/&lt;token&gt;</code> — copy
					the full link the administrator shared with you.
				</p>
				<a class="btn" href="/">Go to the console</a>
			</div>
		</div>
	);
}
