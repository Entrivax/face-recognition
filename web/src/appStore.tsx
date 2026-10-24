// App-wide data context: the signed-in session, the repository list with the
// selected repo, health, the people list of that repo, the match threshold,
// and what the viewer may do (Perms), plus the shared server actions.
//
// MainApp builds the value from its own server-data state; ShareView builds
// a read-only instance (fixed repository, no-op mutating actions). Components
// that need this data call useApp() instead of receiving it as props — the
// re-render granularity is the same as the prop drilling it replaces (the
// value object is rebuilt on every owner render).

import { createContext } from "preact";
import { useContext } from "preact/hooks";
import type { ComponentChildren } from "preact";
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

export interface AppContextValue {
	user: SessionUser | null;
	health: Health | null;
	healthErr: boolean;
	repos: RepoInfo[];
	reposErr: boolean;
	/** the selected repository, or null while none is selected/known yet */
	repoId: string | null;
	/** the selected repository's record (null while unknown) */
	repo: RepoInfo | null;
	people: PersonSummary[];
	peopleErr: boolean;
	/** server-persisted threshold; null = unknown (share mode) → row hidden */
	threshold: number | null;
	perms: Perms;
	setRepoId: (id: string) => void;
	logout: () => Promise<void>;
	/** any mutation → reload the people list, the repo counts and health */
	refreshAll: () => void;
	commitThreshold: (v: number) => Promise<void>;
	rescan: () => Promise<void>;
	removePerson: (name: string) => Promise<void>;
	/** every person-details document a person modal loads (share mode
	 *  learns the repository's human name from it; the app omits it) */
	onPersonDetail?: (d: PersonDetail) => void;
}

const AppContext = createContext<AppContextValue | null>(null);

/** App-wide data + actions; throws when rendered outside a provider. */
export function useApp(): AppContextValue {
	const v = useContext(AppContext);
	if (!v) throw new Error("useApp: no AppProvider above this component");
	return v;
}

export function AppProvider({ value, children }: { value: AppContextValue; children: ComponentChildren }) {
	return <AppContext.Provider value={value}>{children}</AppContext.Provider>;
}
