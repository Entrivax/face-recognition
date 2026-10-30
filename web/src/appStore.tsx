// App-wide data model: the signed-in session, the repository list with the
// selected repo, health, the people list of that repo, the match threshold,
// and what the viewer may do (Perms), plus the shared server actions —
// built as a Preact signals model (createModel/useModel) instead of a
// plain context value. Components read app.people.value etc. and re-render
// per-signal rather than on every owner render.
//
// MainApp creates the writable instance (useModel(() => new AppModel(...)));
// ShareView creates a read-only one (fixed repository, no-op mutating
// actions). useModel is per-mount, so the owner creates the model once and
// shares it through AppProvider — components call useApp() instead of
// receiving it as props. The loading logic lives here too, moved verbatim
// from the former owners.

import { createContext } from "preact";
import { useContext } from "preact/hooks";
import type { ComponentChildren } from "preact";
import { computed, createModel, effect, signal } from "@preact/signals";
import type { ReadonlySignal } from "@preact/signals";
import * as api from "./api";
import { ShareInvalidError } from "./api/core";
import type { ToastKind } from "./toast";
import type { Health, PersonDetail, PersonSummary, RepoInfo, SessionUser } from "./types";

/** What the viewer (signed-in user or share visitor) may do in this repo. */
export interface Perms {
	/** a signed-in user (false for share visitors) */
	user: boolean;
	/** may enroll/modify people in the selected repo (writer or admin) */
	writer: boolean;
	/** server administrator */
	admin: boolean;
}

/** What a model instance needs from its owner; one variant per owner. */
export type AppModelDeps =
	| { mode: "app"; user: SessionUser | null; onSessionEnd: () => void;
	    toast: { show(msg: string, kind?: ToastKind): void } }
	| { mode: "share"; fixedRepoId: string;
	    onPersonDetail?: (d: PersonDetail) => void };

export interface IAppModel {
	user: ReadonlySignal<SessionUser | null>;
	health: ReadonlySignal<Health | null>;
	healthErr: ReadonlySignal<boolean>;
	repos: ReadonlySignal<RepoInfo[]>;
	reposErr: ReadonlySignal<boolean>;
	/** the selected repository id, or null while none is selected/known yet */
	repoId: ReadonlySignal<string | null>;
	/** the selected repository's record (null while unknown) */
	repo: ReadonlySignal<RepoInfo | null>;
	people: ReadonlySignal<PersonSummary[]>;
	peopleErr: ReadonlySignal<boolean>;
	/** server-persisted threshold; null = unknown (share mode) → row hidden */
	threshold: ReadonlySignal<number | null>;
	perms: ReadonlySignal<Perms>;
	/** boot loads: health (+ repos & config in app mode). Owner effect. */
	init(): void;
	/** load the selected repo's people list (supersede-guarded). Resolves
	 *  on failure too (→ peopleErr); rejects only on ShareInvalidError —
	 *  ShareView's boot keys the invalid phase off that rejection. */
	loadPeople(): Promise<void>;
	setRepoId(id: string): void;
	logout(): Promise<void>;
	/** any mutation → reload the people list, the repo counts and health
	 *  (share mode: people + health only) */
	refreshAll(): void;
	commitThreshold(v: number): Promise<void>;
	rescan(): Promise<void>;
	removePerson(name: string): Promise<void>;
	/** every person-details document a person modal loads (share mode
	 *  learns the repository's human name from it; app mode: no-op) */
	onPersonDetail(d: PersonDetail): void;
}

const REPO_KEY = "recogn.repo";

export const AppModel = createModel((deps: AppModelDeps): IAppModel => {
	const share = deps.mode === "share";
	// The signed-in user, captured once at creation: App renders MainApp
	// only while the session probe reports a user and unmounts it before
	// clearing the user, so it cannot change under a live model (share
	// mode has no session at all).
	const user = signal<SessionUser | null>(share ? null : deps.user);
	const health = signal<Health | null>(null);
	const healthErr = signal(false);
	const repos = signal<RepoInfo[]>([]);
	const reposErr = signal(false);
	const repoId = signal<string | null>(share ? deps.fixedRepoId : null);
	const people = signal<PersonSummary[]>([]);
	const peopleErr = signal(false);
	// Global match threshold; null = unknown (still loading / failed) so the
	// panel can hide the row instead of showing a wrong value.
	const threshold = signal<number | null>(null);

	// The selected repo + what the viewer may do in it. Writers and admins
	// may enroll/manage people; only server admins may touch the threshold.
	// (user: MainApp is only rendered signed-in; share visitors get false.)
	const repo = computed<RepoInfo | null>(() => repos.value.find((r) => r.id === repoId.value) ?? null);
	const perms = computed<Perms>(() => ({
		user: !share,
		writer: repo.value ? repo.value.role === "writer" || repo.value.role === "admin" : false,
		admin: !!user.value?.admin,
	}));

	// ---- data loading (moved verbatim from the former owners) ----
	const checkHealth = async () => {
		try {
			const j = await api.getHealth();
			health.value = j;
			healthErr.value = false;
		} catch {
			healthErr.value = true;
		}
	};

	const loadRepos = async () => {
		try {
			const j = await api.listRepos();
			const list = j.repos || [];
			repos.value = list;
			reposErr.value = false;
			// Keep the current selection; when it vanished (or is unset) fall
			// back to the persisted choice, then to the first repo.
			const cur = repoId.peek();
			if (cur && list.some((r) => r.id === cur)) return;
			const stored = localStorage.getItem(REPO_KEY);
			const found = stored ? list.find((r) => r.id === stored) : null;
			repoId.value = (found ?? list[0])?.id ?? null;
		} catch {
			reposErr.value = true;
		}
	};

	// The people fetch is async and the selected repo can change while it is
	// in flight: loadPeople records the repo it fetches for and ignores both
	// the success and the failure of a request that has since been
	// superseded (an older repo's list must not land under the new repo).
	let peopleReq: string | null = null;
	const loadPeople = async () => {
		const want = repoId.value;
		if (!want) {
			peopleReq = null;
			people.value = [];
			return;
		}
		peopleReq = want;
		try {
			const j = await api.getPeople(want);
			if (peopleReq !== want) return; // superseded request
			people.value = j.people || [];
			peopleErr.value = false;
		} catch (e) {
			if (peopleReq !== want) return; // superseded request
			if (e instanceof ShareInvalidError) throw e; // the share hook already flipped the phase
			peopleErr.value = true;
		}
	};

	// GET /api/config is open to any signed-in user — the slider mirrors the
	// server's persisted threshold and readers see the value as read-only.
	const loadConfig = async () => {
		try {
			const c = await api.getConfig();
			threshold.value = c.threshold;
		} catch {
			threshold.value = null;
		}
	};

	// Any mutation (person removed, photos added/deleted, avatar changed,
	// enroll succeeded) reloads the people list, the repo list (people
	// counts) and health.
	const refreshAll = () => {
		void loadPeople().catch(() => {}); // rejects only on share-invalid; the hook owns that flip
		void checkHealth();
		if (!share) void loadRepos();
	};

	// Model-owned effects (createModel disposes them with the model).
	if (share) {
		// Keep the share view's status pill honest: poll health every 30 s.
		effect(() => {
			const t = window.setInterval(() => void checkHealth(), 30000);
			return () => window.clearInterval(t);
		});
	} else {
		// Persist the selection (localStorage "recogn.repo").
		effect(() => {
			const id = repoId.value;
			if (id) localStorage.setItem(REPO_KEY, id);
		});
	}

	return {
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
		init() {
			void checkHealth();
			if (share) return; // share mode loads only health (people list comes from the boot call)
			void loadRepos();
			void loadConfig();
		},
		loadPeople,
		setRepoId(id: string) {
			repoId.value = id;
		},
		async logout() {
			if (deps.mode === "share") return; // nothing to end — no session
			try {
				await api.logout();
				deps.toast.show("Logged out.", "ok");
			} catch {
				deps.toast.show("Logout failed.", "err");
			}
			deps.onSessionEnd();
		},
		refreshAll,
		async commitThreshold(v: number) {
			if (deps.mode === "share") return; // readers cannot change the threshold
			try {
				const j = await api.setThreshold(v);
				threshold.value = j.threshold;
				deps.toast.show(`Threshold set to ${Number(j.threshold).toFixed(2)} (saved).`, "ok");
			} catch (e) {
				deps.toast.show((e as Error).message || "Could not set the threshold.", "err");
			}
		},
		async rescan() {
			if (deps.mode === "share") return; // share visitors cannot rescan
			const id = repoId.value;
			if (!id) return;
			try {
				const j = await api.rescan(id);
				deps.toast.show(`Rescan done: ${j.added} new, ${j.kept} kept, ${j.failed} failed.`, "ok");
				refreshAll();
			} catch (e) {
				deps.toast.show((e as Error).message, "err");
			}
		},
		async removePerson(name: string) {
			if (deps.mode === "share") return; // share visitors cannot remove people
			const id = repoId.value;
			if (!id) return;
			if (!confirm(`Remove ${name} and all their photos from this repository?`)) return;
			try {
				await api.deletePerson(name, id);
				deps.toast.show(`Removed ${name}.`, "ok");
				refreshAll();
			} catch (e) {
				deps.toast.show((e as Error).message, "err");
			}
		},
		onPersonDetail(d: PersonDetail) {
			if (deps.mode === "share") deps.onPersonDetail?.(d);
			// app mode: nothing to learn — the repos list already carries names
		},
	};
});

const AppModelContext = createContext<IAppModel | null>(null);

/** App-wide data + actions; throws when rendered outside a provider. */
export function useApp(): IAppModel {
	const v = useContext(AppModelContext);
	if (!v) throw new Error("useApp: no AppProvider above this component");
	return v;
}

export function AppProvider({ model, children }: { model: IAppModel; children: ComponentChildren }) {
	return <AppModelContext.Provider value={model}>{children}</AppModelContext.Provider>;
}
