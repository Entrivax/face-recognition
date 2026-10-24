// MainApp — the signed-in application shell. App renders it once the
// session probe reports an authenticated user; it owns the server-data
// state (repositories, the selected repo, health, people, threshold) and
// the shared actions, provided to the components through the app-store
// context. Everything dialog-shaped lives in the modal service: components
// open, close, and update modals themselves (see modalRegistry), and the
// provider owns the document-level Escape / clipboard / scroll-lock
// behavior.
//
// Everything data-shaped is scoped to the selected repository: the Topbar's
// selector picks it (persisted in localStorage "recogn.repo"), the people
// list, stage recognition, enrollment and the photo manager all pass its id
// along, and per-repo gating follows the viewer's role in that repo.

import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import * as api from "../api";
import type { Health, PersonSummary, RepoInfo, SessionUser } from "../types";
import { AppProvider, type AppContextValue, type Perms } from "../appStore";
import { ModalProvider } from "../modalService";
import { MODAL_DEFS, useModals, type TypedModalService } from "../modalRegistry";
import { useToast } from "../toast";
import { Topbar } from "./Topbar";
import { Stage } from "./Stage";
import { PeoplePanel } from "./PeoplePanel";

const REPO_KEY = "recogn.repo";

interface MainAppProps {
	user: SessionUser | null;
	/** the session ended (logout) — App flips to the login view */
	onSessionEnd: () => void;
}

export function MainApp(props: MainAppProps) {
	const toast = useToast();

	// ---- server state ----
	const [health, setHealth] = useState<Health | null>(null);
	const [healthErr, setHealthErr] = useState(false);
	const [repos, setRepos] = useState<RepoInfo[]>([]);
	const [reposErr, setReposErr] = useState(false);
	const [repoId, setRepoId] = useState<string | null>(null);
	const [people, setPeople] = useState<PersonSummary[]>([]);
	const [peopleErr, setPeopleErr] = useState(false);
	// Global match threshold; null = unknown (still loading / failed) so the
	// panel can hide the row instead of showing a wrong value.
	const [threshold, setThreshold] = useState<number | null>(null);

	const user = props.user;

	// The selected repo + what the viewer may do in it. Writers and admins
	// may enroll/manage people; only server admins may touch the threshold.
	const repo = repos.find((r) => r.id === repoId) ?? null;
	const perms: Perms = {
		user: true,
		writer: repo ? repo.role === "writer" || repo.role === "admin" : false,
		admin: !!user?.admin,
	};

	// ---- data loading ----
	const checkHealth = useCallback(async () => {
		try {
			const j = await api.getHealth();
			setHealth(j);
			setHealthErr(false);
		} catch {
			setHealthErr(true);
		}
	}, []);

	const loadRepos = useCallback(async () => {
		try {
			const j = await api.listRepos();
			const list = j.repos || [];
			setRepos(list);
			setReposErr(false);
			// Keep the current selection; when it vanished (or is unset) fall
			// back to the persisted choice, then to the first repo.
			setRepoId((cur) => {
				if (cur && list.some((r) => r.id === cur)) return cur;
				const stored = localStorage.getItem(REPO_KEY);
				const found = stored ? list.find((r) => r.id === stored) : null;
				return (found ?? list[0])?.id ?? null;
			});
		} catch {
			setReposErr(true);
		}
	}, []);

	// The people fetch is async and the selected repo can change while it is
	// in flight: loadPeople records the repo it fetches for and ignores both
	// the success and the failure of a request that has since been
	// superseded (an older repo's list must not land under the new repo).
	const peopleReqRef = useRef<string | null>(null);
	const loadPeople = useCallback(async () => {
		const want = repoId;
		if (!want) {
			peopleReqRef.current = null;
			setPeople([]);
			return;
		}
		peopleReqRef.current = want;
		try {
			const j = await api.getPeople(want);
			if (peopleReqRef.current !== want) return; // superseded request
			setPeople(j.people || []);
			setPeopleErr(false);
		} catch {
			if (peopleReqRef.current !== want) return; // superseded request
			setPeopleErr(true);
		}
	}, [repoId]);

	// Any mutation (person removed, photos added/deleted, avatar changed,
	// enroll succeeded) reloads the people list, the repo list (people
	// counts) and health.
	const refreshAll = useCallback(() => {
		void loadPeople();
		void checkHealth();
		void loadRepos();
	}, [loadPeople, checkHealth, loadRepos]);

	// GET /api/config is open to any signed-in user — the slider mirrors the
	// server's persisted threshold and readers see the value as read-only.
	const loadConfig = useCallback(async () => {
		try {
			const c = await api.getConfig();
			setThreshold(c.threshold);
		} catch {
			setThreshold(null);
		}
	}, []);

	// ---- boot ----
	useEffect(() => {
		void checkHealth();
		void loadRepos();
		void loadConfig();
	}, [checkHealth, loadRepos, loadConfig]);

	// Reload the people list whenever the repository changes.
	useEffect(() => {
		void loadPeople();
	}, [loadPeople]);

	// Persist the selection (localStorage "recogn.repo").
	useEffect(() => {
		if (repoId) localStorage.setItem(REPO_KEY, repoId);
	}, [repoId]);

	// ---- actions ----
	const doLogout = useCallback(async () => {
		try {
			await api.logout();
			toast.show("Logged out.", "ok");
		} catch {
			toast.show("Logout failed.", "err");
		}
		props.onSessionEnd();
	}, [props.onSessionEnd, toast]);

	const commitThreshold = useCallback(async (v: number) => {
		try {
			const j = await api.setThreshold(v);
			setThreshold(j.threshold);
			toast.show(`Threshold set to ${Number(j.threshold).toFixed(2)} (saved).`, "ok");
		} catch (e) {
			toast.show((e as Error).message || "Could not set the threshold.", "err");
		}
	}, [toast]);

	const doRescan = useCallback(async () => {
		if (!repoId) return;
		try {
			const j = await api.rescan(repoId);
			toast.show(`Rescan done: ${j.added} new, ${j.kept} kept, ${j.failed} failed.`, "ok");
			refreshAll();
		} catch (e) {
			toast.show((e as Error).message, "err");
		}
	}, [repoId, refreshAll, toast]);

	const removePerson = useCallback(async (name: string) => {
		if (!repoId) return;
		if (!confirm(`Remove ${name} and all their photos from this repository?`)) return;
		try {
			await api.deletePerson(name, repoId);
			toast.show(`Removed ${name}.`, "ok");
			refreshAll();
		} catch (e) {
			toast.show((e as Error).message, "err");
		}
	}, [repoId, refreshAll]);

	// A repository switch must not leave data modals pointing at another
	// repo's people. The compare tool is repo-independent and stays.
	const closeRepoModals = useCallback((modals: TypedModalService) => {
		modals.close("enroll", "person", "photos");
	}, []);

	const store: AppContextValue = {
		user,
		health,
		healthErr,
		repos,
		reposErr,
		repoId,
		repo,
		people,
		peopleErr,
		threshold,
		perms,
		setRepoId,
		logout: doLogout,
		refreshAll,
		commitThreshold,
		rescan: doRescan,
		removePerson,
	};

	return (
		<AppProvider value={store}>
			{/* Inner: pooled modal instances must see the store. RepoBridge
			  closes the repo-scoped dialogs on a repository switch. */}
			<ModalProvider defs={MODAL_DEFS}>
				<RepoBridge repoId={repoId} onRepoSwitch={closeRepoModals} />
				<Topbar />
				{reposErr ? (
					<main class="layout">
						<div class="empty-state" role="alert">
							<h2>Could not load repositories</h2>
							<p class="lede">The server did not answer the repository list — it may be restarting.</p>
							<button type="button" class="btn" onClick={() => void loadRepos()}>Try again</button>
						</div>
					</main>
				) : repos.length === 0 ? (
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
				) : repoId ? (
					<main class="layout">
						<Stage canEnroll={perms.writer} />
						<PeoplePanel perms={perms} />
					</main>
				) : null}
			</ModalProvider>
		</AppProvider>
	);
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

// RepoBridge reacts to repository switches (MainApp re-renders it on every
// store rebuild): data modals that point at another repo's people are
// closed. The compare tool is repo-independent and stays open.
function RepoBridge({ repoId, onRepoSwitch }: {
	repoId: string | null;
	onRepoSwitch: (modals: TypedModalService) => void;
}) {
	const modals = useModals();
	const prev = useRef<string | null | undefined>(undefined);
	useEffect(() => {
		if (prev.current !== undefined && repoId !== prev.current) {
			onRepoSwitch(modals);
		}
		prev.current = repoId;
	}, [repoId, modals, onRepoSwitch]);
	return null;
}
