// MainApp — the signed-in application shell. App renders it once the
// session probe reports an authenticated user; it owns the cross-cutting
// state (repositories, the selected repo, health, people, threshold, which
// modals are open), the clipboard routing, and the Escape-stack keydown
// handler. Children refresh each other via refreshAll() callbacks.
//
// Everything data-shaped is scoped to the selected repository: the Topbar's
// selector picks it (persisted in localStorage "recogn.repo"), the people
// list, stage recognition, enrollment and the photo manager all pass its id
// along, and per-repo gating follows the viewer's role in that repo.

import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import * as api from "../api";
import type { CheckView, Health, PersonSummary, RepoInfo, SessionUser } from "../types";
import { handleEscape } from "../modalStack";
import { imageFilesFromClipboard, plural } from "../util";
import { useToast } from "../toast";
import { Topbar } from "./Topbar";
import { Stage } from "./Stage";
import { PeoplePanel, type Perms } from "./PeoplePanel";
import { EnrollModal, type EnrollPasteTarget } from "./EnrollModal";
import { PersonModal } from "./PersonModal";
import { PhotosModal } from "./PhotosModal";
import { FaceCheckModal } from "./FaceCheckModal";
import { CompareModal, type ComparePasteTarget, type CompareSeed } from "./CompareModal";
import { ReposModal } from "./ReposModal";
import { UsersModal } from "./UsersModal";
import { ApiKeysModal } from "./ApiKeysModal";
import { ChangePasswordModal } from "./ChangePasswordModal";

const REPO_KEY = "recogn.repo";

interface MainAppProps {
	user: SessionUser;
	/** the session ended (logout) — App flips to the login view */
	onSessionEnd: () => void;
}

export function MainApp({ user, onSessionEnd }: MainAppProps) {
	const toast = useToast();
	const toastRef = useRef(toast);
	toastRef.current = toast;

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

	// ---- modal state ----
	const [enrollOpen, setEnrollOpen] = useState(false);
	const [compareOpen, setCompareOpen] = useState(false);
	// Both photos of a comparison launched from the enroll or photos modal
	// (picked there); CompareModal consumes the seed when it opens.
	const [compareSeed, setCompareSeed] = useState<CompareSeed | null>(null);
	// Clicking a person opens the details modal; the photos manager opens
	// from there, stacked on top.
	const [detailsName, setDetailsName] = useState<string | null>(null);
	const [photosName, setPhotosName] = useState<string | null>(null);
	const [checkView, setCheckView] = useState<CheckView | null>(null);
	const [reposOpen, setReposOpen] = useState(false);
	const [usersOpen, setUsersOpen] = useState(false);
	const [keysOpen, setKeysOpen] = useState(false);
	const [passwordOpen, setPasswordOpen] = useState(false);
	// The stage photo editor (owned by Stage) reports its visibility here so
	// the scroll lock and the clipboard routing account for it.
	const [editOpen, setEditOpen] = useState(false);

	// The selected repo + what the viewer may do in it. Writers and admins
	// may enroll/manage people; only server admins may touch the threshold.
	const repo = repos.find((r) => r.id === repoId) ?? null;
	const perms: Perms = {
		user: true,
		writer: repo ? repo.role === "writer" || repo.role === "admin" : false,
		admin: user.admin,
	};

	// Clipboard routing targets registered by the children.
	const inspectRef = useRef<((file: File) => void) | null>(null);
	const photosAddRef = useRef<((files: FileList | File[]) => void) | null>(null);
	const enrollPasteRef = useRef<EnrollPasteTarget | null>(null);
	const comparePasteRef = useRef<ComparePasteTarget | null>(null);

	const registerInspect = useCallback((fn: ((file: File) => void) | null) => {
		inspectRef.current = fn;
	}, []);
	const registerAddFiles = useCallback((fn: ((files: FileList | File[]) => void) | null) => {
		photosAddRef.current = fn;
	}, []);
	const registerPasteTarget = useCallback((t: EnrollPasteTarget | null) => {
		enrollPasteRef.current = t;
	}, []);
	const registerComparePaste = useCallback((t: ComparePasteTarget | null) => {
		comparePasteRef.current = t;
	}, []);

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
		loadPeople();
		checkHealth();
		loadRepos();
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
		checkHealth();
		loadRepos();
		loadConfig();
	}, [checkHealth, loadRepos, loadConfig]);

	// Reload the people list whenever the repository changes.
	useEffect(() => {
		loadPeople();
	}, [loadPeople]);

	// Persist the selection (localStorage "recogn.repo").
	useEffect(() => {
		if (repoId) localStorage.setItem(REPO_KEY, repoId);
	}, [repoId]);

	// A repository switch must not leave data modals pointing at another
	// repo's people. The compare tool is repo-independent and stays.
	useEffect(() => {
		setDetailsName(null);
		setPhotosName(null);
		setEnrollOpen(false);
	}, [repoId]);

	// ---- actions ----
	const doLogout = useCallback(async () => {
		try {
			await api.logout();
			toastRef.current.show("Logged out.", "ok");
		} catch {
			toastRef.current.show("Logout failed.", "err");
		}
		onSessionEnd();
	}, [onSessionEnd]);

	const commitThreshold = async (v: number) => {
		try {
			const j = await api.setThreshold(v);
			setThreshold(j.threshold);
			toast.show(`Threshold set to ${Number(j.threshold).toFixed(2)} (saved).`, "ok");
		} catch (e) {
			toast.show((e as Error).message || "Could not set the threshold.", "err");
		}
	};

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

	const removePerson = async (name: string) => {
		if (!repoId) return;
		if (!confirm(`Remove ${name} and all their photos from this repository?`)) return;
		try {
			await api.deletePerson(name, repoId);
			toast.show(`Removed ${name}.`, "ok");
			refreshAll();
		} catch (e) {
			toast.show((e as Error).message, "err");
		}
	};

	// A comparison launched from another modal (enroll pending thumbs /
	// photos-manager grid): both photos were picked there and are handed
	// over as a seed — CompareModal fills its slots and auto-runs.
	const openCompareSeeded = useCallback((a: File, b: File) => {
		setCompareSeed({ a, b });
		setCompareOpen(true);
	}, []);

	// ---- keyboard: Escape closes the topmost open dialog ----
	useEffect(() => {
		const onKey = (e: KeyboardEvent) => {
			if (e.key === "Escape") handleEscape();
		};
		document.addEventListener("keydown", onKey);
		return () => document.removeEventListener("keydown", onKey);
	}, []);

	// ---- body scroll lock while any modal is open ----
	useEffect(() => {
		const open = enrollOpen || compareOpen || detailsName !== null || photosName !== null ||
			checkView !== null || reposOpen || usersOpen || keysOpen || passwordOpen || editOpen;
		document.body.classList.toggle("modal-open", open);
	}, [enrollOpen, compareOpen, detailsName, photosName, checkView, reposOpen, usersOpen, keysOpen, passwordOpen, editOpen]);

	// ---- clipboard routing ----
	// Ctrl+V / Cmd+V routes by context: with the compare modal open it sits
	// on top of whichever modal launched it (or standalone), so it wins the
	// clipboard; with the enroll modal open, pasted images join the review
	// list; with the photos manager open they upload straight into that
	// person; otherwise they are inspected on the stage. While the stage
	// photo editor is open, pasted images are swallowed — the editor sits
	// over the stage and its session must not be reset under the user.
	// Plain-text pastes into inputs are never hijacked.
	useEffect(() => {
		const onPaste = (e: ClipboardEvent) => {
			const files = imageFilesFromClipboard(e.clipboardData);
			if (!files.length) return; // normal text paste — leave to the browser

			const t = e.target as HTMLElement | null;
			if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA") &&
					typeof e.clipboardData?.getData === "function" &&
					e.clipboardData.getData("text/plain")) {
				return; // text field + text on the clipboard: default behaviour wins
			}
			e.preventDefault();

			if (editOpen) {
				toast.show("Close the editor to inspect a new photo.");
				return;
			}

			const enroll = enrollPasteRef.current;
			if (compareOpen && comparePasteRef.current) {
				if (comparePasteRef.current.isBusy()) return;
				comparePasteRef.current.addFiles(files);
			} else if (enrollOpen && enroll) {
				if (enroll.isBusy()) return;
				enroll.addPending(files);
				enroll.pulseDrop();
				enroll.scrollLastThumb();
				toast.show(`Added ${plural(files.length)} from clipboard.`, "ok");
			} else if (photosName !== null && photosAddRef.current) {
				photosAddRef.current(files);
			} else {
				inspectRef.current?.(files[0]);
				if (files.length > 1) toast.show("Clipboard had several images — inspecting the first.");
			}
		};
		document.addEventListener("paste", onPaste);
		return () => document.removeEventListener("paste", onPaste);
	}, [enrollOpen, photosName, compareOpen, editOpen, toast]);

	// A face check finished in the enroll modal: live-update the enlarged
	// viewer when it is showing that photo.
	const onCheckFaces = useCallback((src: string, faces: CheckView["faces"]) => {
		setCheckView((v) => (v && v.src === src ? { ...v, faces } : v));
	}, []);

	return (
		<>
			<Topbar
				health={health}
				err={healthErr}
				user={user}
				repos={repos}
				repoId={repoId}
				onRepoChange={setRepoId}
				onLogout={doLogout}
				onRepos={() => setReposOpen(true)}
				onUsers={() => setUsersOpen(true)}
				onKeys={() => setKeysOpen(true)}
				onPassword={() => setPasswordOpen(true)}
			/>

			{reposErr ? (
				<main class="layout">
					<div class="empty-state" role="alert">
						<h2>Could not load repositories</h2>
						<p class="lede">The server did not answer the repository list — it may be restarting.</p>
						<button type="button" class="btn" onClick={loadRepos}>Try again</button>
					</div>
				</main>
			) : repos.length === 0 ? (
				<main class="layout">
					<div class="empty-state">
						{user.admin ? (
							<>
								<h2>No repositories yet</h2>
								<p class="lede">A repository is a dataset of people. Create the first one, then enroll people into it.</p>
								<button type="button" id="emptyCreateRepoBtn" class="btn btn-accent" onClick={() => setReposOpen(true)}>
									Create a repository
								</button>
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
					<Stage
						canEnroll={perms.writer}
						repoId={repoId}
						onEnrolled={refreshAll}
						registerInspect={registerInspect}
						onEditOpenChange={setEditOpen}
					/>
					<PeoplePanel
						perms={perms}
						people={people}
						peopleErr={peopleErr}
						threshold={threshold}
						onThresholdCommit={commitThreshold}
						onRescan={doRescan}
						onOpenPerson={setDetailsName}
						onRemove={removePerson}
						onEnrollClick={() => setEnrollOpen(true)}
						onCompareClick={() => setCompareOpen(true)}
					/>
				</main>
			) : null}

			<EnrollModal
				open={enrollOpen && perms.writer}
				repoId={repoId ?? ""}
				peopleNames={people.map((p) => p.name)}
				onCloseRequest={() => setEnrollOpen(false)}
				onChange={refreshAll}
				onCheck={setCheckView}
				onCheckFaces={onCheckFaces}
				onComparePhotos={openCompareSeeded}
				registerPasteTarget={registerPasteTarget}
			/>

			<PersonModal
				person={detailsName}
				repoId={repoId ?? ""}
				canManage={perms.writer}
				thumb={detailsName ? people.find((p) => p.name === detailsName)?.thumb ?? null : null}
				onCloseRequest={() => setDetailsName(null)}
				onManagePhotos={setPhotosName}
				onChange={refreshAll}
			/>

			<PhotosModal
				person={perms.writer ? photosName : null}
				repoId={repoId ?? ""}
				onCloseRequest={() => setPhotosName(null)}
				onRenamed={(canonical) => {
					// The rename happened in the photos manager; the details
					// modal underneath follows the person too.
					setPhotosName(canonical);
					setDetailsName((d) => (d !== null ? canonical : d));
				}}
				onChange={refreshAll}
				onComparePhotos={openCompareSeeded}
				registerAddFiles={registerAddFiles}
			/>

			<CompareModal
				open={compareOpen && perms.user}
				onCloseRequest={() => setCompareOpen(false)}
				registerPasteTarget={registerComparePaste}
				seed={compareSeed}
				onSeedConsumed={() => setCompareSeed(null)}
			/>

			<FaceCheckModal view={checkView} onCloseRequest={() => setCheckView(null)} />

			<ReposModal
				open={reposOpen && user.admin}
				onCloseRequest={() => setReposOpen(false)}
				onChanged={refreshAll}
			/>

			<UsersModal
				open={usersOpen && user.admin}
				currentUser={user.name}
				onCloseRequest={() => setUsersOpen(false)}
			/>

			<ApiKeysModal
				open={keysOpen}
				onCloseRequest={() => setKeysOpen(false)}
			/>

			<ChangePasswordModal
				open={passwordOpen}
				onCloseRequest={() => setPasswordOpen(false)}
			/>
		</>
	);
}
