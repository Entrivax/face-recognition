// Enroll modal.
// Photos chosen for enrollment are held client-side for review; the upload
// only happens when the user confirms with the "Enroll" button. More photos
// can be added at any time by drag & drop, paste (routed by the modal
// service), or the file input. Each pending photo immediately gets a
// server-side face check (sequential — inference is a single serialized
// stream) so the user sees the detected faces before committing to
// enrollment.
//
// An optional collapsed "Person details" section collects the person's
// metadata (aliases, birthdate, links, notes) — the same fields and edit
// model as the person-details modal's edit form. It saves right after the
// photos enroll, through POST /api/people/{name}/meta; for an existing
// person the stored details are pre-filled first, because that endpoint has
// full-replace semantics.
//
// Any two pending photos can also be compared face-to-face: each thumb has
// a compare button — the first click marks the photo (A), the second pick
// opens the compare modal with both (seeded, auto-running the similarity
// check). The enlarged face-check viewer opens the same way, stacked on
// top; its faces are patched in as checks complete.
//
// All state and behavior live in EnrollViewModel (createModel/useModel, the
// appStore/LoginView pattern): the form signals, signal-backed element
// refs, the computeds, every action, and the model-owned effects (the
// details pre-fill, the beforeunload guard, the preview-URL revocation —
// disposed with the model). Returned actions are batched and untracked, so
// the async chains (the check queue, the enrollment request, the details
// pre-fill) read the values they guard at call time — pending.value/peek(),
// enrolling.value, cmpKey.value, metaTouched.peek(), name.peek() — and
// nothing can act on a removed photo or a superseded name. Non-reactive
// plumbing (the pre-fill watermark, the serialized check chain, the pulse
// timer) stays in plain closure locals, not state. The component keeps only
// the injected lifecycle props: open/isTopmost stay plain; onCloseRequest
// reaches the model through useLiveSignal; paste routing stays in paste.ts.

import { useEffect, useRef } from "preact/hooks";
import { batch, computed, createModel, effect, signal, useModel } from "@preact/signals";
import type { ReadonlySignal, Signal } from "@preact/signals";
import { For, useLiveSignal } from "@preact/signals/utils";
import type { TargetedDragEvent, TargetedEvent } from "preact";
import * as api from "../api";
import type { Face } from "../types";
import { BIRTH_FORMATS_HINT, formatBirthText, parseBirthText } from "../birthdate";
import { drawFaces } from "../overlay";
import { fmtSize, plural } from "../util";
import { useToast, type ToastKind } from "../toast";
import { useApp, type IAppModel } from "../appStore";
import { useModals, type TypedModalService } from "../modalRegistry";
import { usePasteFiles } from "../paste";
import { MetaRows } from "./MetaRows";
import { Modal } from "./Modal";

const NAME_HINT = "Shown as the identity when their face is recognized.";

// Metadata limits mirrored from the server (internal/api/meta.go) so the
// combined form fails before any photo is sent.
const MAX_ALIASES = 20;
const MAX_URLS = 50;

interface PendingPhoto {
	key: string;
	file: File;
	url: string;
	faces: Face[] | null;
	status: "checking" | "checked" | "error";
	failed?: boolean;
	note?: string;
}

interface EnrollModalProps {
	/** injected by the modal service (instances render open) */
	open: boolean;
	/** injected by the modal service: true while no other instance sits above */
	isTopmost: boolean;
	/** injected by the modal service; closing unmounts the instance */
	onCloseRequest: () => void;
}

/** What the model needs from its owner; wired per mount in the component. */
interface EnrollModelDeps {
	app: IAppModel;
	toast: { show(msg: string, kind?: ToastKind): void };
	modals: TypedModalService;
	/** the service-injected close request (live-updated per render) */
	onCloseRequest: ReadonlySignal<() => void>;
}

interface IEnrollModel {
	// ---- form state ----
	name: Signal<string>;
	pending: Signal<PendingPhoto[]>;
	/** the enrollment request is in flight (Modal.locked + disabled attrs) */
	enrolling: Signal<boolean>;
	/** the pending photo currently marked as comparison photo A */
	cmpKey: Signal<string | null>;
	// ---- person details section ----
	aliases: Signal<string[]>;
	urls: Signal<string[]>;
	birthField: Signal<string>;
	description: Signal<string>;
	// ---- element refs (signal-backed; JSX attaches via ref callbacks) ----
	nameInputRef: Signal<HTMLInputElement | null>;
	birthInputRef: Signal<HTMLInputElement | null>;
	dropInputRef: Signal<HTMLInputElement | null>;
	thumbsRef: Signal<HTMLUListElement | null>;
	detailsRef: Signal<HTMLDetailsElement | null>;
	// ---- derived ----
	nameHint: ReadonlySignal<string>;
	nameHintClass: ReadonlySignal<string>;
	meta: ReadonlySignal<string>;
	noPending: ReadonlySignal<boolean>;
	submitDisabled: ReadonlySignal<boolean>;
	submitLabel: ReadonlySignal<string>;
	enrollDropClass: ReadonlySignal<string>;
	// ---- actions ----
	onAliasesChange(v: string[]): void;
	onUrlsChange(v: string[]): void;
	onBirthInput(v: string): void;
	onDescriptionInput(v: string): void;
	onDragEnter(e: TargetedDragEvent<HTMLDivElement>): void;
	onDragOver(e: TargetedDragEvent<HTMLDivElement>): void;
	onDragLeave(e: TargetedDragEvent<HTMLDivElement>): void;
	onDrop(e: TargetedDragEvent<HTMLDivElement>): void;
	/** paste handler, registered by the component while it is topmost */
	onPaste(files: File[]): void;
	/** add files to the review list (no upload) */
	addPending(fileList: FileList | File[]): void;
	removePending(file: File): void;
	pickCompare(p: PendingPhoto): void;
	/** open the enlarged face-check viewer for one pending photo */
	inspect(p: PendingPhoto): void;
	openPicker(): void;
	submit(e: TargetedEvent<HTMLFormElement>): Promise<void>;
	requestClose(): void;
}

const EnrollViewModel = createModel<IEnrollModel, [EnrollModelDeps]>((deps: EnrollModelDeps): IEnrollModel => {
	const { app, toast, modals } = deps;

	// ---- form state ----
	const name = signal("");
	const pending = signal<PendingPhoto[]>([]);
	const enrolling = signal(false);
	// Pending photo currently marked as photo A of a comparison.
	const cmpKey = signal<string | null>(null);

	// Optional person details, saved right after the photos enroll (the meta
	// endpoint needs the person to exist). Same edit model as the person
	// details modal: row lists with a trailing empty slot, one dashed
	// birthdate text field, markdown notes.
	const aliases = signal<string[]>([""]);
	const urls = signal<string[]>([""]);
	const birthField = signal("");
	const description = signal("");
	// Touched = the user edited the details section (vs. a programmatic
	// pre-fill): the save then carries the fields even when everything is
	// empty, so clearing a pre-filled value really clears it. Not exposed —
	// only the actions, the pre-fill guard and the dirty computed read it.
	const metaTouched = signal(false);

	// Render-invisible state (only computeds/actions read them).
	const submitMeta = signal<string | null>(null);
	const dropHint = signal(false); // .drag on the dropzone
	const pulse = signal(false); // brief highlight on paste

	// Element refs, signal-backed (the view-model convention): JSX attaches
	// them via ref callbacks; actions read .value at call time.
	const nameInputRef = signal<HTMLInputElement | null>(null);
	const birthInputRef = signal<HTMLInputElement | null>(null);
	const dropInputRef = signal<HTMLInputElement | null>(null);
	const thumbsRef = signal<HTMLUListElement | null>(null);
	const detailsRef = signal<HTMLDetailsElement | null>(null);

	// Non-reactive plumbing — plain closure locals, not state: the pre-fill
	// watermark ("did we pre-fill for this name"), the serialized check
	// chain and the pulse timer.
	let prefilled: string | null = null;
	let chain: Promise<void> = Promise.resolve();
	let pulseTimer: number | undefined = undefined;

	// Empty the details section back to a fresh state (one trailing empty
	// row per list, closed disclosure).
	const resetDetails = () => {
		batch(() => {
			aliases.value = [""];
			urls.value = [""];
			birthField.value = "";
			description.value = "";
			metaTouched.value = false;
		});
		prefilled = null;
		if (detailsRef.value) detailsRef.value.open = false;
	};

	// User edits mark the section touched; pre-fill never does. (Returned
	// actions are batched by createModel — no explicit batch() needed.)
	const onAliasesChange = (v: string[]) => { metaTouched.value = true; aliases.value = v; };
	const onUrlsChange = (v: string[]) => { metaTouched.value = true; urls.value = v; };
	const onBirthInput = (v: string) => { metaTouched.value = true; birthField.value = v; };
	const onDescriptionInput = (v: string) => { metaTouched.value = true; description.value = v; };

	// ---- derived state ----
	const metaFilled = computed(() =>
		aliases.value.some((a) => a.trim() !== "") || urls.value.some((u) => u.trim() !== "") ||
		birthField.value.trim() !== "" || description.value.trim() !== "");
	const dirty = computed(() =>
		pending.value.length > 0 || name.value.trim() !== "" || metaTouched.value || metaFilled.value);
	const known = computed(() => Boolean(name.value.trim()) &&
		app.people.value.some((x) => x.name.toLowerCase() === name.value.trim().toLowerCase()));
	const nameHint = computed(() => known.value
		? `${name.value.trim()} is already enrolled — photos will be added to their profile; their details are pre-filled in the section below.`
		: NAME_HINT);
	const nameHintClass = computed(() => "field-hint" + (known.value ? " warn" : ""));
	const n = computed(() => pending.value.length);
	const bytes = computed(() => pending.value.reduce((s, p) => s + p.file.size, 0));
	const meta = computed(() => submitMeta.value ?? (n.value
		? `${plural(n.value)} ready · ${fmtSize(bytes.value)} · review, then enroll`
		: "No photos selected yet."));
	const noPending = computed(() => n.value === 0);
	const submitDisabled = computed(() => !n.value || !name.value.trim());
	const submitLabel = computed(() => enrolling.value
		? "Enrolling…"
		: n.value
			? `Enroll ${n.value} photo${n.value === 1 ? "" : "s"}`
			: "Enroll photos");
	const canDrop = computed(() => !enrolling.value);
	const enrollDropClass = computed(() =>
		"enroll-drop" + (enrolling.value ? " disabled" : "") +
		(dropHint.value ? " drag" : "") + (pulse.value ? " pulse" : ""));

	// The instance unmounts on close: drop the preview URLs with it (the
	// review list dies with the model, so everything else was per-open
	// state already). A dependency-free effect runs once; its cleanup runs
	// when the model is disposed.
	effect(() => () => {
		for (const p of pending.peek()) URL.revokeObjectURL(p.url);
	});

	// Pre-fill the details when the typed name matches an enrolled person:
	// the meta save is a full-replace, so the form must show exactly what
	// would be saved. Best-effort — a failed fetch leaves the fields as they
	// are. When the name stops matching, drop a stale pre-fill unless the
	// user has edited the fields since (their edits win). The effect tracks
	// the typed name only: the enrolled-people list and the repo are read
	// with peek(), so a background refresh does not re-run the pre-fill.
	effect(() => {
		const trimmed = name.value.trim();
		const knownNow = trimmed !== "" &&
			app.people.peek().some((x) => x.name.toLowerCase() === trimmed.toLowerCase());
		if (!knownNow) {
			// peek(): metaTouched must not become a tracked dependency — the
			// effect re-runs on the typed name only.
			if (prefilled !== null && !metaTouched.peek()) resetDetails();
			return;
		}
		let stale = false;
		api.getPerson(trimmed, app.repoId.peek() ?? "").then((j) => {
			if (stale || name.peek() !== trimmed || metaTouched.peek()) return;
			prefilled = trimmed;
			batch(() => {
				aliases.value = [...(j.aliases ?? []), ""];
				urls.value = [...(j.urls ?? []), ""];
				birthField.value = formatBirthText(j.birthdate);
				description.value = j.description ?? "";
			});
		}).catch(() => {
			// The save is a full-replace: warn when the section may not show
			// everything stored for this person, so nothing is wiped unseen.
			if (stale || name.peek() !== trimmed) return;
			toast.show(`Could not load ${trimmed}'s current details — the section below may not show everything stored for them.`, "err");
		});
		return () => { stale = true; };
	});

	// Warn before an unload that would lose an in-progress review.
	effect(() => {
		if (!dirty.value) return;
		const onUnload = (e: BeforeUnloadEvent) => {
			e.preventDefault();
			e.returnValue = ""; // legacy Chrome/Safari; modern engines ignore custom text
		};
		window.addEventListener("beforeunload", onUnload);
		return () => window.removeEventListener("beforeunload", onUnload);
	});

	// ---- pre-submit face check ----
	// Each pending photo is sent to /api/recognize as soon as it joins the
	// list (sequentially — inference is a single serialized stream) so the
	// user sees the detected faces before deciding to enroll.
	const updatePending = (target: PendingPhoto, patch: Partial<PendingPhoto>) => {
		pending.value = pending.value.map((p) => (p === target ? { ...p, ...patch } : p));
	};

	const checkPendingFace = async (p: PendingPhoto) => {
		if (!pending.value.includes(p)) return; // removed while queued
		try {
			const j = await api.recognize(p.file, app.repoId.value ?? "");
			if (!pending.value.includes(p)) return;
			const faces = j.faces || [];
			updatePending(p, { faces, status: "checked" });
			// Live-update the enlarged viewer when it is showing this photo
			// (patch no-ops while no check instance is open).
			modals.patch("check", { view: { src: p.url, title: p.file.name, faces } },
				(cur) => cur?.view?.src === p.url);
		} catch {
			if (!pending.value.includes(p)) return;
			updatePending(p, { status: "error" });
		}
	};

	// ---- pending photos ----

	// Add files to the review list (no upload). Dedupes by name+size+mtime.
	function addPending(fileList: FileList | File[]) {
		let added = 0, notImage = 0, duplicate = 0;
		const additions: PendingPhoto[] = [];
		for (const file of Array.from(fileList)) {
			if (!file.type.startsWith("image/")) { notImage++; continue; }
			const key = `${file.name}\n${file.size}\n${file.lastModified}`;
			if (pending.value.some((p) => p.key === key)) { duplicate++; continue; }
			additions.push({ key, file, url: URL.createObjectURL(file), faces: null, status: "checking" });
			added++;
		}
		if (additions.length) {
			pending.value = [...pending.value, ...additions];
			for (const p of additions) {
				chain = chain
					.then(() => checkPendingFace(p))
					.catch(() => {});
			}
		}
		if (notImage) toast.show(`${notImage} file(s) skipped — not images.`, "err");
		if (duplicate && !added) toast.show(`${duplicate} file(s) already selected.`);
	}

	function removePending(file: File) {
		const list = pending.value;
		const i = list.findIndex((p) => p.file === file);
		if (i < 0) return;
		URL.revokeObjectURL(list[i].url);
		if (cmpKey.value === list[i].key) cmpKey.value = null;
		pending.value = list.filter((p) => p.file !== file);
	}

	// ---- compare two pending photos ----
	// First pick marks the photo as photo A; picking the marked photo again
	// cancels; picking a different photo opens the compare modal with both
	// (seeded — it fills its slots and auto-runs the similarity check).
	function pickCompare(p: PendingPhoto) {
		if (enrolling.value) return;
		if (!cmpKey.value) {
			cmpKey.value = p.key;
			toast.show(`${p.file.name} set as photo A — pick another photo to compare.`, "ok");
			return;
		}
		if (cmpKey.value === p.key) {
			cmpKey.value = null;
			return;
		}
		const a = pending.value.find((x) => x.key === cmpKey.value);
		cmpKey.value = null;
		if (a) modals.open("compare", { seed: { a: a.file, b: p.file } });
	}

	// Enlarged face-check viewer for one pending photo (stacked on top).
	const inspect = (p: PendingPhoto) =>
		modals.open("check", { view: { src: p.url, title: p.file.name, faces: p.faces } });

	// ---- dropzone (drag & drop + file picker) ----
	const openPicker = () => {
		if (!enrolling.value) dropInputRef.value?.click();
	};
	const onDragEnter = (e: TargetedDragEvent<HTMLDivElement>) => { e.preventDefault(); if (canDrop.value) dropHint.value = true; };
	const onDragOver = (e: TargetedDragEvent<HTMLDivElement>) => { e.preventDefault(); if (canDrop.value) dropHint.value = true; };
	const onDragLeave = (e: TargetedDragEvent<HTMLDivElement>) => {
		if (!e.relatedTarget) dropHint.value = false;
	};
	const onDrop = (e: TargetedDragEvent<HTMLDivElement>) => {
		e.preventDefault();
		dropHint.value = false;
		if (canDrop.value && e.dataTransfer?.files?.length) addPending(e.dataTransfer.files);
	};

	// ---- clipboard paste (the component registers this while topmost) ----
	const onPaste = (files: File[]) => {
		if (enrolling.value) return; // busy swallows the paste silently
		addPending(files);
		pulse.value = true;
		clearTimeout(pulseTimer);
		pulseTimer = window.setTimeout(() => { pulse.value = false; }, 900);
		thumbsRef.value?.lastElementChild?.scrollIntoView({ block: "nearest" });
		toast.show(`Added ${plural(files.length)} from clipboard.`, "ok");
	};

	const requestClose = () => {
		if (enrolling.value) return; // locked while the request is in flight
		deps.onCloseRequest.value();
	};

	const submit = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (enrolling.value) return;
		const person = name.value.trim();
		if (!person) {
			toast.show("Enter the person's name first.", "err");
			nameInputRef.value?.focus();
			return;
		}
		if (!pending.value.length) {
			toast.show("Add at least one photo.", "err");
			return;
		}

		// Validate the details fully client-side so a bad field never sends
		// the photos; the server re-checks everything when the meta save runs.
		const birth = parseBirthText(birthField.value);
		if (!birth.ok) {
			toast.show(birth.error, "err");
			birthInputRef.value?.focus();
			return;
		}
		const parts = birth.parts;
		const birthdate = parts.year == null && parts.month == null && parts.day == null
			? null
			: { year: parts.year ?? null, month: parts.month ?? null, day: parts.day ?? null };
		const aliasList = aliases.value.map((a) => a.trim()).filter(Boolean);
		const urlList = urls.value.map((u) => u.trim()).filter(Boolean);
		if (aliasList.length > MAX_ALIASES) {
			toast.show(`Too many aliases (max ${MAX_ALIASES}).`, "err");
			return;
		}
		if (urlList.length > MAX_URLS) {
			toast.show(`Too many URLs (max ${MAX_URLS}).`, "err");
			return;
		}
		// A touched section sends even when everything is now empty (a
		// deliberate clear must clear); otherwise only non-empty fields do.
		const hasMeta = metaTouched.value || aliasList.length > 0 || urlList.length > 0 ||
			birthdate !== null || description.value.trim() !== "";

		batch(() => {
			enrolling.value = true;
			submitMeta.value = `Enrolling ${pending.value.length} photo${pending.value.length === 1 ? "" : "s"} for ${person}…`;
		});

		try {
			const j = await api.enrollPhotos(person, app.repoId.value ?? "", pending.value.map((p) => p.file));

			if (j.added > 0) toast.show(`Enrolled ${j.added} photo${j.added === 1 ? "" : "s"} for ${person}.`, "ok");
			app.refreshAll();

			// The details save after the photos because the meta endpoint
			// needs the person to exist; its failure must not lose the
			// enrollment result.
			if (hasMeta && j.added > 0) {
				submitMeta.value = `Saving details for ${person}…`;
				try {
					await api.setPersonMeta(person, app.repoId.value ?? "", {
						aliases: aliasList,
						birthdate,
						urls: urlList,
						description: description.value,
					});
					toast.show("Details saved.", "ok");
					app.refreshAll(); // the people list summary carries aliases
				} catch (err) {
					const why = (err as Error).message || "could not save the details";
					toast.show(`Photos enrolled, but the details could not be saved (${why}) — edit them later in the person's details.`, "err");
				}
			}

			// Keep the modal open when some photos failed, so they can be swapped.
			const failures = j.failures || [];
			const failedNames = new Set(failures
				.map((f) => (f.includes(":") ? f.slice(0, f.indexOf(":")) : f).trim())
				.filter(Boolean));
			const kept: PendingPhoto[] = [];
			for (const p of pending.value) {
				if (!failedNames.has(p.file.name)) {
					URL.revokeObjectURL(p.url);
					continue;
				}
				const raw = failures.find((f) => f.startsWith(p.file.name + ":")) || "";
				const reason = raw.slice(p.file.name.length + 1).trim() || "could not be enrolled";
				kept.push({ ...p, failed: true, note: reason.includes("no face detected") ? "no face detected" : reason });
			}

			batch(() => {
				pending.value = kept;
				enrolling.value = false;
			});
			if (kept.length) {
				submitMeta.value = null;
				toast.show(`${kept.length} photo(s) had no detectable face — remove or replace them.`, "err");
			} else {
				requestClose();
			}
		} catch (err) {
			batch(() => {
				enrolling.value = false;
				submitMeta.value = null;
			});
			toast.show((err as Error).message || "Enroll failed.", "err");
		}
	};

	return {
		name, pending, enrolling, cmpKey,
		aliases, urls, birthField, description,
		nameInputRef, birthInputRef, dropInputRef, thumbsRef, detailsRef,
		nameHint, nameHintClass, meta, noPending, submitDisabled, submitLabel, enrollDropClass,
		onAliasesChange, onUrlsChange, onBirthInput, onDescriptionInput,
		onDragEnter, onDragOver, onDragLeave, onDrop,
		onPaste, addPending, removePending, pickCompare, inspect, openPicker,
		submit, requestClose,
	};
});

export function EnrollModal(props: EnrollModalProps) {
	const { open, isTopmost } = props;
	const app = useApp();
	const toast = useToast();
	const modals = useModals();
	const onCloseRequest = useLiveSignal(props.onCloseRequest);
	const model = useModel(() => new EnrollViewModel({ app, toast, modals, onCloseRequest }));

	// While this modal is topmost, pasted images join the review list.
	usePasteFiles(isTopmost, model.onPaste);

	return (
		<Modal
			open={open}
			id="enrollModal"
			cardId="enrollCard"
			locked={model.enrolling}
			eyebrow="enrollment"
			titleId="enrollTitle"
			title="Enroll a new person"
			initialFocus={() => model.nameInputRef.value}
			backdropClickDisabled={true}
			onCloseRequest={model.requestClose}
			rootProps={{
				onDragEnter: model.onDragEnter,
				onDragOver: model.onDragOver,
				onDragLeave: model.onDragLeave,
				onDrop: model.onDrop,
			}}
			body={
				<form id="enrollForm" class="modal-body" novalidate onSubmit={model.submit}>
					<label class="field-label" for="enrollName">Name</label>
					<input
						ref={(el) => { model.nameInputRef.value = el; }}
						id="enrollName"
						type="text"
						placeholder="Person's name"
						autocomplete="off"
						spellcheck={false}
						disabled={model.enrolling}
						value={model.name}
						onInput={(e) => { model.name.value = e.currentTarget.value; }}
					/>
					<p class={model.nameHintClass} id="enrollNameHint">{model.nameHint}</p>

					<details ref={(el) => { model.detailsRef.value = el; }} id="enrollDetails" class="enroll-details">
						<summary>Person details — aliases, birthdate, links, notes (optional)</summary>
						<div class="enroll-details-body">
							<div class="enroll-field">
								<span class="field-label">Aliases</span>
								<MetaRows entries={model.aliases} onChange={model.onAliasesChange} label="Alias" placeholder="Alias" maxLength={120} disabled={model.enrolling.value} />
							</div>

							<div class="enroll-field">
								<label class="field-label" for="enrollBirth">Birthdate — leave empty if unknown</label>
								<input
									ref={(el) => { model.birthInputRef.value = el; }}
									id="enrollBirth"
									type="text"
									placeholder="1965-03-02"
									autocomplete="off"
									spellcheck={false}
									disabled={model.enrolling}
									value={model.birthField}
									onInput={(e) => model.onBirthInput(e.currentTarget.value)}
								/>
								<p class="field-hint">{BIRTH_FORMATS_HINT}</p>
							</div>

							<div class="enroll-field">
								<span class="field-label">Links</span>
								<MetaRows entries={model.urls} onChange={model.onUrlsChange} label="URL" placeholder="https://…" maxLength={2048} disabled={model.enrolling.value} />
							</div>

							<div class="enroll-field">
								<label class="field-label" for="enrollNotes">Notes — markdown supported</label>
								<textarea
									id="enrollNotes"
									rows={4}
									maxLength={20000}
									placeholder="Free-form description…"
									disabled={model.enrolling}
									value={model.description}
									onInput={(e) => model.onDescriptionInput(e.currentTarget.value)}
								/>
							</div>
						</div>
					</details>

					<div
						id="enrollDrop"
						class={model.enrollDropClass}
						tabindex={0}
						role="button"
						aria-label="Add photos — drag and drop, paste, or press Enter to browse"
						onClick={model.openPicker}
						onKeyDown={(e) => {
							if ((e.key === "Enter" || e.key === " ") && !model.enrolling.value) {
								e.preventDefault();
								model.openPicker();
							}
						}}
					>
						<svg viewBox="0 0 48 48" class="edz-icon" aria-hidden="true">
							<path
								d="M15 8h-5a2 2 0 0 0-2 2v5M33 8h5a2 2 0 0 1 2 2v5M15 40h-5a2 2 0 0 1-2-2v-5M33 40h5a2 2 0 0 0 2-2v-5"
								fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"
							/>
							<circle cx="24" cy="21" r="5" fill="none" stroke="currentColor" stroke-width="2" />
							<path
								d="M15 36c2-4.5 5.4-6 9-6s7 1.6 9 6"
								fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"
							/>
						</svg>
						<p class="edz-primary">Drag &amp; drop, paste, or <span class="edz-browse">browse</span></p>
						<p class="edz-hint">Nothing uploads until you review and confirm</p>
						<input
							ref={(el) => { model.dropInputRef.value = el; }}
							id="enrollPhotos"
							type="file"
							accept="image/*"
							multiple
							hidden
							onChange={(e) => {
								if (e.currentTarget.files?.length) model.addPending(e.currentTarget.files);
								e.currentTarget.value = ""; // allow re-picking the same file later
							}}
						/>
					</div>

					<ul ref={(el) => { model.thumbsRef.value = el; }} id="enrollThumbs" class="enroll-thumbs" hidden={model.noPending}>
						<For each={model.pending} getKey={(p) => p.key}>
							{(p) => (
								<EnrollThumb
									p={p}
									cmpKey={model.cmpKey}
									onRemove={() => model.removePending(p.file)}
									onInspect={() => model.inspect(p)}
									onCompare={() => model.pickCompare(p)}
								/>
							)}
						</For>
					</ul>
					<p class="enroll-meta" id="enrollMeta">{model.meta}</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close>Cancel</button>
						<button type="submit" id="enrollSubmit" class="btn btn-accent" disabled={model.submitDisabled}>
							{model.submitLabel}
						</button>
					</footer>
				</form>
			}
		/>
	);
}

// One pending-photo tile: preview with a face-check overlay canvas, status
// badge, compare button (marks the photo as photo A of a face-to-face
// comparison) and remove button. Clicking the tile inspects the faces
// full-size; clicks on the corner buttons never trigger the inspect action.
// The tile subscribes to the compare-mark signal itself (a cmpKey change
// re-renders only the affected tiles).
function EnrollThumb(props: {
	p: PendingPhoto;
	/** the pending photo currently marked as comparison photo A (read here) */
	cmpKey: ReadonlySignal<string | null>;
	onRemove: () => void;
	onInspect: () => void;
	onCompare: () => void;
}) {
	const { p } = props;
	const imgRef = useRef<HTMLImageElement | null>(null);
	const canvasRef = useRef<HTMLCanvasElement | null>(null);
	const cmpMark = props.cmpKey.value === p.key;

	// Face boxes are drawn over the thumbnail as soon as detection returns.
	useEffect(() => {
		if (p.status !== "checked" || !p.faces) return;
		const img = imgRef.current;
		const canvas = canvasRef.current;
		if (!img || !canvas) return;
		const draw = () => drawFaces(canvas, img, p.faces || [], { labels: false });
		if (img.complete && img.naturalWidth) draw();
		else img.onload = draw;
	}, [p.status, p.faces]);

	let badgeText = "checking…";
	let badgeCls = "face-badge wait";
	if (p.status === "error") {
		badgeText = "check failed";
		badgeCls = "face-badge warn";
	} else if (p.status === "checked") {
		const n = p.faces?.length ?? 0;
		if (n === 0) { badgeText = "no face"; badgeCls = "face-badge warn"; }
		else if (n === 1) { badgeText = "1 face"; badgeCls = "face-badge ok"; }
		else { badgeText = `${n} faces`; badgeCls = "face-badge multi"; }
	}

	return (
		<li
			class={"enroll-thumb" + (p.failed ? " failed" : "") + (cmpMark ? " cmp-mark" : "")}
			tabIndex={0}
			title="Click to inspect faces"
			onClick={(e) => {
				if ((e.target as Element).closest?.(".enroll-thumb-del, .enroll-thumb-cmp")) return;
				props.onInspect();
			}}
			onKeyDown={(e) => {
				if ((e.target as Element).closest?.(".enroll-thumb-del, .enroll-thumb-cmp")) return;
				if (e.key === "Enter" || e.key === " ") { e.preventDefault(); props.onInspect(); }
			}}
		>
			<div class="thumb-wrap">
				<img ref={imgRef} src={p.url} alt="" />
				<canvas ref={canvasRef} />
			</div>
			<span class={badgeCls}>{badgeText}</span>
			<button
				type="button"
				class="enroll-thumb-cmp"
				aria-label={`Compare ${p.file.name} with another photo`}
				aria-pressed={cmpMark}
				title={cmpMark
					? "Photo A selected — click again to cancel, or pick another photo"
					: "Compare with another pending photo"}
				onClick={props.onCompare}
			>
				<svg viewBox="0 0 48 48" aria-hidden="true">
					<path
						d="M15 8h-5a2 2 0 0 0-2 2v5M33 8h5a2 2 0 0 1 2 2v5M15 40h-5a2 2 0 0 1-2-2v-5M33 40h5a2 2 0 0 0 2-2v-5"
						fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round"
					/>
					<circle cx="24" cy="21" r="5" fill="none" stroke="currentColor" stroke-width="2.6" />
					<path
						d="M15 36c2-4.5 5.4-6 9-6s7 1.6 9 6"
						fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round"
					/>
				</svg>
			</button>
			{cmpMark && <span class="cmp-badge">A</span>}
			<button
				type="button"
				class="enroll-thumb-del"
				aria-label={`Remove ${p.file.name}`}
				onClick={props.onRemove}
			>
				×
			</button>
			<div class="enroll-thumb-meta">
				<div class="enroll-thumb-name" title={p.file.name}>{p.file.name}</div>
				<div class="enroll-thumb-size">{fmtSize(p.file.size)}</div>
			</div>
			{p.note !== undefined && <p class="enroll-thumb-note">{p.note}</p>}
		</li>
	);
}
