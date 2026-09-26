// App owns the cross-cutting state (health, people, threshold, which modals
// are open), the clipboard routing, and the Escape-stack keydown handler.
// Cross-module refreshes that the old code wired via on*Change callbacks are
// plain refreshAll() calls now.

import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import * as api from "../api";
import type { CheckView, Health, PersonSummary } from "../types";
import { handleEscape } from "../modalStack";
import { imageFilesFromClipboard, plural } from "../util";
import { useToast } from "../toast";
import { Topbar } from "./Topbar";
import { Stage } from "./Stage";
import { PeoplePanel } from "./PeoplePanel";
import { EnrollModal, type EnrollPasteTarget } from "./EnrollModal";
import { PhotosModal } from "./PhotosModal";
import { FaceCheckModal } from "./FaceCheckModal";
import { CompareModal, type ComparePasteTarget, type CompareSeed } from "./CompareModal";
import { LoginModal } from "./LoginModal";
import { PasskeysModal } from "./PasskeysModal";

export function App() {
	const toast = useToast();
	// Used by the (mount-stable) unauthorized hook below.
	const toastRef = useRef(toast);
	toastRef.current = toast;

	// ---- server state ----
	const [health, setHealth] = useState<Health | null>(null);
	const [healthErr, setHealthErr] = useState(false);
	const [people, setPeople] = useState<PersonSummary[]>([]);
	const [peopleErr, setPeopleErr] = useState(false);
	const [threshold, setThreshold] = useState(0.45);
	const [authed, setAuthed] = useState(false);

	// ---- modal state ----
	const [enrollOpen, setEnrollOpen] = useState(false);
	const [compareOpen, setCompareOpen] = useState(false);
	// Both photos of a comparison launched from the enroll or photos modal
	// (picked there); CompareModal consumes the seed when it opens.
	const [compareSeed, setCompareSeed] = useState<CompareSeed | null>(null);
	const [photosName, setPhotosName] = useState<string | null>(null);
	const [checkView, setCheckView] = useState<CheckView | null>(null);
	const [loginOpen, setLoginOpen] = useState(false);
	const [passkeysOpen, setPasskeysOpen] = useState(false);
	// The stage photo editor (owned by Stage) reports its visibility here so
	// the scroll lock and the clipboard routing account for it.
	const [editOpen, setEditOpen] = useState(false);

	// Which admin-login methods the server has configured (none = open mode).
	const authMethods = health?.auth ?? { password: false, passkey: false };
	// Admin-only surfaces render only while logged in.
	const enrollVisible = enrollOpen && authed;
	const compareVisible = compareOpen && authed;
	const photosPerson = authed ? photosName : null;

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

	const loadPeople = useCallback(async () => {
		try {
			const j = await api.getPeople();
			setPeople(j.people || []);
			setPeopleErr(false);
		} catch {
			setPeopleErr(true);
		}
	}, []);

	// Any mutation (person removed, photos added/deleted, avatar changed,
	// enroll succeeded) reloads the people list and health.
	const refreshAll = useCallback(() => {
		loadPeople();
		checkHealth();
	}, [loadPeople, checkHealth]);

	// /api/config is admin-only: fetched once the session check says we're
	// logged in (boot) and again after a successful login.
	const loadConfig = useCallback(async () => {
		try {
			const c = await api.getConfig();
			setThreshold(c.threshold);
		} catch {
			// Stay on the default; the status pill shows the live value either way.
		}
	}, []);

	// ---- boot ----
	useEffect(() => {
		checkHealth();
		loadPeople();
		api.getSession()
			.then((s) => {
				setAuthed(s.authenticated);
				// The slider mirrors the server's persisted threshold; the
				// status pill shows the live value either way.
				if (s.authenticated) loadConfig();
			})
			.catch(() => setAuthed(false));
		const t = window.setInterval(checkHealth, 30000);
		return () => window.clearInterval(t);
	}, [checkHealth, loadPeople, loadConfig]);

	// Any admin wrapper that catches a 401 flips the app to logged-out — the
	// session expired and the next admin call would fail anyway.
	const onUnauthorized = useCallback(() => {
		setAuthed(false);
		toastRef.current.show("Session expired — log in again.", "err");
	}, []);

	useEffect(() => {
		api.setOnUnauthorized(onUnauthorized);
		return () => api.setOnUnauthorized(null);
	}, [onUnauthorized]);

	// ---- actions ----
	const doLogout = useCallback(async () => {
		try {
			await api.logout();
			toastRef.current.show("Logged out.", "ok");
		} catch {
			toastRef.current.show("Logout failed.", "err");
		}
		setAuthed(false);
	}, []);

	// Credentials accepted: sync the admin-only config, then refresh data.
	const onLoggedIn = useCallback(() => {
		setAuthed(true);
		loadConfig();
		refreshAll();
	}, [loadConfig, refreshAll]);
	const commitThreshold = async (v: number) => {
		try {
			const j = await api.setThreshold(v);
			setThreshold(j.threshold);
			toast.show(`Threshold set to ${Number(j.threshold).toFixed(2)} (saved).`, "ok");
			checkHealth();
		} catch (e) {
			toast.show((e as Error).message || "Could not set the threshold.", "err");
		}
	};

	const doRescan = useCallback(async () => {
		try {
			const j = await api.rescan();
			toast.show(`Rescan done: ${j.added} new, ${j.kept} kept, ${j.failed} failed.`, "ok");
			refreshAll();
		} catch (e) {
			toast.show((e as Error).message, "err");
		}
	}, [refreshAll, toast]);

	const removePerson = async (name: string) => {
		if (!confirm(`Remove ${name} and all their photos from the database?`)) return;
		try {
			await api.deletePerson(name);
			toast.show(`Removed ${name}.`, "ok");
			loadPeople();
			checkHealth();
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
		document.body.classList.toggle(
			"modal-open",
			enrollVisible || compareVisible || photosPerson !== null || checkView !== null || loginOpen || passkeysOpen || editOpen,
		);
	}, [enrollVisible, compareVisible, photosPerson, checkView, loginOpen, passkeysOpen, editOpen]);

	// ---- clipboard routing ----
	// Ctrl+V / Cmd+V routes by context: with the compare modal open it sits
	// on top of whichever modal launched it (or standalone), so it wins the
	// clipboard; with the enroll modal open, pasted images join the review
	// list; with the photos manager open they upload straight into that
	// person; otherwise (including when logged out — the admin targets are
	// unavailable then) they are inspected on the stage.
	// While the stage photo editor is open, pasted images are swallowed —
	// the editor sits over the stage and its session must not be reset under
	// the user. Plain-text pastes into inputs are never hijacked.
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
			if (compareVisible && comparePasteRef.current) {
				if (comparePasteRef.current.isBusy()) return;
				comparePasteRef.current.addFiles(files);
			} else if (enrollVisible && enroll) {
				if (enroll.isBusy()) return;
				enroll.addPending(files);
				enroll.pulseDrop();
				enroll.scrollLastThumb();
				toast.show(`Added ${plural(files.length)} from clipboard.`, "ok");
			} else if (photosPerson !== null && photosAddRef.current) {
				photosAddRef.current(files);
			} else {
				inspectRef.current?.(files[0]);
				if (files.length > 1) toast.show("Clipboard had several images — inspecting the first.");
			}
		};
		document.addEventListener("paste", onPaste);
		return () => document.removeEventListener("paste", onPaste);
	}, [enrollVisible, photosPerson, compareVisible, editOpen, toast]);

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
				authed={authed}
				authMethods={authMethods}
				onLogin={() => setLoginOpen(true)}
				onLogout={doLogout}
				onPasskeys={() => setPasskeysOpen(true)}
			/>

			<main class="layout">
				<Stage
					authed={authed}
					onEnrolled={refreshAll}
					registerInspect={registerInspect}
					onEditOpenChange={setEditOpen}
				/>
				<PeoplePanel
					authed={authed}
					people={people}
					peopleErr={peopleErr}
					threshold={threshold}
					onThresholdCommit={commitThreshold}
					onRescan={doRescan}
					onOpenPhotos={setPhotosName}
					onRemove={removePerson}
					onEnrollClick={() => setEnrollOpen(true)}
					onCompareClick={() => setCompareOpen(true)}
				/>
			</main>

			<EnrollModal
				open={enrollVisible}
				peopleNames={people.map((p) => p.name)}
				onCloseRequest={() => setEnrollOpen(false)}
				onChange={refreshAll}
				onCheck={setCheckView}
				onCheckFaces={onCheckFaces}
				onComparePhotos={openCompareSeeded}
				registerPasteTarget={registerPasteTarget}
			/>

			<PhotosModal
				person={photosPerson}
				onCloseRequest={() => setPhotosName(null)}
				onRenamed={setPhotosName}
				onChange={refreshAll}
				onComparePhotos={openCompareSeeded}
				registerAddFiles={registerAddFiles}
			/>

			<CompareModal
				open={compareVisible}
				onCloseRequest={() => setCompareOpen(false)}
				registerPasteTarget={registerComparePaste}
				seed={compareSeed}
				onSeedConsumed={() => setCompareSeed(null)}
			/>

			<FaceCheckModal view={checkView} onCloseRequest={() => setCheckView(null)} />

			<LoginModal
				open={loginOpen}
				health={health}
				onCloseRequest={() => setLoginOpen(false)}
				onLoggedIn={onLoggedIn}
			/>

			<PasskeysModal
				open={passkeysOpen && authed}
				onCloseRequest={() => setPasskeysOpen(false)}
			/>
		</>
	);
}
