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
//
// The share tree is provided with the same app-store / modal-service
// context pair as MainApp: a read-only store (fixed repository, no-op
// mutating actions, its own repo document) and the shared modal registry —
// the details modal opens through the service like everywhere else. Boot
// phase is a signal; the repository's human name and the threshold arrive
// with the boot repo document (app.repoName / app.threshold). Stage,
// PeoplePanel and StatusPill read the store themselves (no props).

import { useEffect } from "preact/hooks";
import { useComputed, useModel, useSignal, type ReadonlySignal } from "@preact/signals";
import { setOnShareInvalid, setShareContext } from "../api/core";
import { AppModel, AppProvider } from "../appStore";
import { ModalProvider } from "../modalService";
import { MODAL_DEFS } from "../modalRegistry";
import { StatusPill } from "./Topbar";
import { Stage } from "./Stage";
import { PeoplePanel } from "./PeoplePanel";

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
	// Boot phase (which card renders); the repository's human name rides the
	// store's repoName signal (set by the boot loadRepo).
	const phase = useSignal<SharePhase>("loading");

	// Attach the share context to every API call and clear it again on
	// unmount (a visitor who then somehow re-enters the app must not keep
	// the header). 401/403 anywhere flip the phase to the error card.
	useEffect(() => {
		setShareContext({ repoId, token });
		setOnShareInvalid(() => { phase.value = "invalid"; });
		return () => {
			setShareContext(null);
			setOnShareInvalid(null);
		};
	}, [repoId, token]);

	// The read-only store handed to the share tree: fixed repository, no
	// mutating actions. GET /api/repo carries the repository's human name
	// and the threshold at boot (the repos list needs a session).
	const app = useModel(() => new AppModel({ mode: "share", fixedRepoId: repoId }));

	// Boot: verify the token by loading the repo's info document (the model
	// also polls health every 30 s). A rejected load is a ShareInvalidError —
	// the hook above has already flipped the phase; anything else resolves
	// with peopleErr set.
	useEffect(() => {
		void app.init();
		app.loadRepo().then(
			() => { phase.value = "ready"; },
			() => {}, // invalid token — the hook already flipped the phase
		);
	}, [app]);

	// Banner display name: the server-reported human name, else the short
	// repo id. A computed so the load's result swaps it into the banner.
	const bannerName = useComputed(() => app.repoName.value ?? shortRepoId(repoId));

	// The phase chain reads phase.value in render (a tracked read), so a
	// flip re-renders exactly one branch: the loading/invalid cards stand
	// alone; the ready tree mounts the providers around the propless
	// children. (Three exclusive phases with a provider-carrying fall-through
	// — the chain is cleaner here than nested Shows.)
	if (phase.value === "loading") {
		return (
			<div class="auth-page">
				<div class="auth-card">
					<p class="auth-lede" aria-live="polite">Loading shared repository…</p>
				</div>
			</div>
		);
	}

	if (phase.value === "invalid") {
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

	return (
		<AppProvider model={app}>
			<ModalProvider defs={MODAL_DEFS}>
				<ShareTree repoId={repoId} repoName={bannerName} />
			</ModalProvider>
		</AppProvider>
	);
}

// The visible share surface, inside the providers (the modal instances see
// the store). The person-details dialog is opened by the people panel
// through the modal service; the status pill and the panels read the store
// from context, and the banner name rides the signal.
function ShareTree({ repoId, repoName }: {
	repoId: string;
	repoName: ReadonlySignal<string>;
}) {
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
					<span class="share-banner-label">Repository</span>
					<span class="share-banner-name" title={repoId}>{repoName}</span>
				</div>
				<StatusPill />
			</header>

			<main class="layout">
				<Stage />
				<PeoplePanel />
			</main>
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
