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

export function App() {
	const toast = useToast();

	// ---- server state ----
	const [health, setHealth] = useState<Health | null>(null);
	const [healthErr, setHealthErr] = useState(false);
	const [people, setPeople] = useState<PersonSummary[]>([]);
	const [peopleErr, setPeopleErr] = useState(false);
	const [threshold, setThreshold] = useState(0.45);

	// ---- modal state ----
	const [enrollOpen, setEnrollOpen] = useState(false);
	const [photosName, setPhotosName] = useState<string | null>(null);
	const [checkView, setCheckView] = useState<CheckView | null>(null);

	// Clipboard routing targets registered by the children.
	const inspectRef = useRef<((file: File) => void) | null>(null);
	const photosAddRef = useRef<((files: FileList | File[]) => void) | null>(null);
	const enrollPasteRef = useRef<EnrollPasteTarget | null>(null);

	const registerInspect = useCallback((fn: ((file: File) => void) | null) => {
		inspectRef.current = fn;
	}, []);
	const registerAddFiles = useCallback((fn: ((files: FileList | File[]) => void) | null) => {
		photosAddRef.current = fn;
	}, []);
	const registerPasteTarget = useCallback((t: EnrollPasteTarget | null) => {
		enrollPasteRef.current = t;
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

	// ---- boot ----
	useEffect(() => {
		checkHealth();
		loadPeople();
		// The slider mirrors the server's persisted threshold; the status pill
		// shows the live value either way.
		api.getConfig().then((c) => setThreshold(c.threshold)).catch(() => {});
		const t = window.setInterval(checkHealth, 30000);
		return () => window.clearInterval(t);
	}, [checkHealth, loadPeople]);

	// ---- actions ----
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
		document.body.classList.toggle("modal-open", enrollOpen || photosName !== null || checkView !== null);
	}, [enrollOpen, photosName, checkView]);

	// ---- clipboard routing ----
	// Ctrl+V / Cmd+V routes by context: with the enroll modal open, pasted
	// images join the review list; with the photos manager open they upload
	// straight into that person; otherwise they are inspected on the stage.
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

			const enroll = enrollPasteRef.current;
			if (enrollOpen && enroll) {
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
	}, [enrollOpen, photosName, toast]);

	// A face check finished in the enroll modal: live-update the enlarged
	// viewer when it is showing that photo.
	const onCheckFaces = useCallback((src: string, faces: CheckView["faces"]) => {
		setCheckView((v) => (v && v.src === src ? { ...v, faces } : v));
	}, []);

	return (
		<>
			<Topbar health={health} err={healthErr} />

			<main class="layout">
				<Stage onEnrolled={refreshAll} registerInspect={registerInspect} />
				<PeoplePanel
					people={people}
					peopleErr={peopleErr}
					threshold={threshold}
					onThresholdCommit={commitThreshold}
					onRescan={doRescan}
					onOpenPhotos={setPhotosName}
					onRemove={removePerson}
					onEnrollClick={() => setEnrollOpen(true)}
				/>
			</main>

			<EnrollModal
				open={enrollOpen}
				peopleNames={people.map((p) => p.name)}
				onCloseRequest={() => setEnrollOpen(false)}
				onChange={refreshAll}
				onCheck={setCheckView}
				onCheckFaces={onCheckFaces}
				registerPasteTarget={registerPasteTarget}
			/>

			<PhotosModal
				person={photosName}
				onCloseRequest={() => setPhotosName(null)}
				onRenamed={setPhotosName}
				onChange={refreshAll}
				registerAddFiles={registerAddFiles}
			/>

			<FaceCheckModal view={checkView} onCloseRequest={() => setCheckView(null)} />
		</>
	);
}
