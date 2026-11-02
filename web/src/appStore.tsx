// App-wide data model: the signed-in session, the repository list with the
// selected repo, health, the selected repo's info document (people list,
// human name, threshold), and what the viewer may do (Perms), plus the
// shared server actions — built as a Preact signals model
// (createModel/useModel) instead of a plain context value. Components read
// app.people.value etc. and re-render per-signal rather than on every owner
// render.
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
import type { Health, PersonSummary, RepoInfo, SessionUser } from "./types";

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
	| { mode: "share"; fixedRepoId: string };

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
	/** the selected repository's human name as the server reports it (null
	 *  while unknown — the share banner falls back to the short repo id) */
	repoName: ReadonlySignal<string | null>;
	/** server-persisted threshold; null = unknown (still loading / failed) */
	threshold: ReadonlySignal<number | null>;
	perms: ReadonlySignal<Perms>;
	/** boot loads: health (+ repos in app mode). Owner effect. */
	init(): void;
	/** load the selected repo's info document — people list, name,
	 *  threshold (supersede-guarded). Resolves on failure too (→ peopleErr);
	 *  rejects only on ShareInvalidError — ShareView's boot keys the
	 *  invalid phase off that rejection. */
	loadRepo(): Promise<void>;
	setRepoId(id: string): void;
	logout(): Promise<void>;
	/** any mutation → reload the repo document, the repo counts and health
	 *  (share mode: repo document + health only) */
	refreshAll(): void;
	commitThreshold(v: number): Promise<void>;
	rescan(): Promise<void>;
	removePerson(name: string): Promise<void>;
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
	// The selected repository's human name (from GET /api/repo); the share
	// banner falls back to the short repo id while null.
	const repoName = signal<string | null>(null);
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

	// The repo fetch is async and the selected repo can change while it is
	// in flight: loadRepo records the repo it fetches for and ignores both
	// the success and the failure of a request that has since been
	// superseded (an older repo's document must not land under the new
	// repo). One GET /api/repo round trip carries the people list, the
	// repository's human name and the threshold.
	let repoReq: string | null = null;
	const loadRepo = async () => {
		const want = repoId.value;
		if (!want) {
			repoReq = null;
			people.value = [];
			repoName.value = null;
			return;
		}
		if (repoReq !== want) repoName.value = null; // new repo: drop the stale name
		repoReq = want;
		try {
			const j = await api.getRepo(want);
			if (repoReq !== want) return; // superseded request
			people.value = j.people || [];
			repoName.value = j.name;
			threshold.value = j.threshold;
			peopleErr.value = false;
		} catch (e) {
			if (repoReq !== want) return; // superseded request
			if (e instanceof ShareInvalidError) throw e; // the share hook already flipped the phase
			peopleErr.value = true;
		}
	};

	// Any mutation (person removed, photos added/deleted, avatar changed,
	// enroll succeeded) reloads the repo document, the repo list (people
	// counts) and health.
	const refreshAll = () => {
		void loadRepo().catch(() => {}); // rejects only on share-invalid; the hook owns that flip
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
		repoName,
		threshold,
		perms,
		init() {
			void checkHealth();
			if (share) return; // share mode loads only health (the repo document comes from the boot call)
			void loadRepos();
		},
		loadRepo,
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
