// MainApp — the signed-in application shell. App renders it once the
// session probe reports an authenticated user; it creates the app store
// model (signals + server data + actions, see appStore) and provides it to
// the components through the app-store context. Everything dialog-shaped
// lives in the modal service: components open, close, and update modals
// themselves (see modalRegistry), and the provider owns the document-level
// Escape / clipboard / scroll-lock behavior.
//
// MainApp's own render reads nothing reactive: the store signals are read in
// the smallest components (RepoBridge watches the selected repo, AppLayout
// branches on the repo state, Stage/PeoplePanel read their permissions from
// the store themselves).

import { useEffect, useRef } from "preact/hooks";
import { useModel, useSignalEffect } from "@preact/signals";
import type { SessionUser } from "../types";
import { AppModel, AppProvider, useApp } from "../appStore";
import { ModalProvider } from "../modalService";
import { MODAL_DEFS, useModals } from "../modalRegistry";
import { useToast } from "../toast";
import { Topbar } from "./Topbar";
import { Stage } from "./Stage";
import { PeoplePanel } from "./PeoplePanel";

interface MainAppProps {
	user: SessionUser | null;
	/** the session ended (logout) — App flips to the login view */
	onSessionEnd: () => void;
}

export function MainApp(props: MainAppProps) {
	const toast = useToast();
	// The store model: created once per mount, disposed with it. props.user
	// is constant while MainApp is mounted (App unmounts it to change it)
	// and onSessionEnd is semantically stable (it flips App's own boot
	// signals), so both are captured once at creation.
	const app = useModel(() => new AppModel({ mode: "app", user: props.user, onSessionEnd: props.onSessionEnd, toast }));

	// Boot loads, then reload the people list whenever the repository
	// changes (loadPeople is an action — untracked reads — so the tracked
	// repoId read here drives the effect).
	useEffect(() => { app.init(); }, [app]);
	useSignalEffect(() => {
		void app.repoId.value;
		void app.loadPeople();
	});

	return (
		<AppProvider model={app}>
			{/* Inner: pooled modal instances must see the store. RepoBridge
			  closes the repo-scoped dialogs on a repository switch. */}
			<ModalProvider defs={MODAL_DEFS}>
				<RepoBridge />
				<Topbar />
				<AppLayout />
			</ModalProvider>
		</AppProvider>
	);
}

// The main layout: an error card, an empty card (admin: call to action), or
// the stage + people panel for the selected repository. Reads the store
// signals it branches on (a reload re-renders this component only);
// Stage/PeoplePanel subscribe to their own signals.
function AppLayout() {
	const app = useApp();
	const reposErr = app.reposErr.value;
	const repos = app.repos.value;
	const repoId = app.repoId.value;
	const user = app.user.value;

	if (reposErr) {
		return (
			<main class="layout">
				<div class="empty-state" role="alert">
					<h2>Could not load repositories</h2>
					<p class="lede">The server did not answer the repository list — it may be restarting.</p>
					<button type="button" class="btn" onClick={() => app.refreshAll()}>Try again</button>
				</div>
			</main>
		);
	}
	if (repos.length === 0) {
		return (
			<main class="layout">
				<div class="empty-state">
					{user?.admin ? (
						<>
							<h2>No repositories yet</h2>
							<p class="lede">A repository is a dataset of people. Create the first one, then enroll people into it.</p>
							<CreateRepoButton />
						</>
					) : (
						<>
							<h2>No repositories yet</h2>
							<p class="lede">You have not been granted access to any repository — ask the administrator for access.</p>
						</>
					)}
				</div>
			</main>
		);
	}
	if (!repoId) return null;
	return (
		<main class="layout">
			<Stage />
			<PeoplePanel />
		</main>
	);
}

// A repository switch must not leave data modals pointing at another
// repo's people. The compare tool is repo-independent and stays open.
function RepoBridge() {
	const app = useApp();
	const modals = useModals();
	const prev = useRef<string | null | undefined>(undefined);
	useSignalEffect(() => {
		const repoId = app.repoId.value;
		if (prev.current !== undefined && repoId !== prev.current) {
			modals.close("enroll", "person", "photos");
		}
		prev.current = repoId;
	});
	return null;
}

// The empty-state call to action (an admin with zero repositories): the
// Repositories manager, opened through the modal service.
function CreateRepoButton() {
	const modals = useModals();
	return (
		<button type="button" id="emptyCreateRepoBtn" class="btn btn-accent" onClick={() => modals.open("repos")}>
			Create a repository
		</button>
	);
}
