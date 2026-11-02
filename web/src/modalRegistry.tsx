// Modal registry: the app's modal keys, their arg shapes, and the component
// definitions handed to every ModalProvider (MainApp and ShareView share the
// same set). useModals() is the typed facade over the generic pool service —
// the single cast at that seam; every call site stays key-typed.
//
// Lifecycle props (open / onCloseRequest / isTopmost) are injected by the
// service and must never be passed through args. Closing a modal unmounts
// its instance; an instance only stays mounted (paused, state intact) while
// another modal sits on top of it.

import type { ComponentType } from "preact";
import type { CheckView } from "./types";
import { useModalService } from "./modalService";
import type { CompareSeed } from "./components/CompareModal";
import type { EditSession } from "./components/EditModal";
import { EnrollModal } from "./components/EnrollModal";
import { PersonModal } from "./components/PersonModal";
import { PhotosModal } from "./components/PhotosModal";
import { CompareModal } from "./components/CompareModal";
import { FaceCheckModal } from "./components/FaceCheckModal";
import { EditModal } from "./components/EditModal";
import { CameraModal } from "./components/CameraModal";
import { ReposModal } from "./components/ReposModal";
import { UsersModal } from "./components/UsersModal";
import { ApiKeysModal } from "./components/ApiKeysModal";
import { ChangePasswordModal } from "./components/ChangePasswordModal";

export type ModalKey =
	| "enroll" | "person" | "photos" | "compare" | "check" | "camera"
	| "edit" | "repos" | "users" | "keys" | "password";

/** Per-key args accepted by open/patch. */
export interface ModalArgs {
	enroll: Record<string, never>;
	/** the person whose details are shown */
	person: { name: string };
	/** the person whose photos are managed */
	photos: { person: string };
	/** seed = both photos picked in the launching modal (auto-runs on open) */
	compare: { seed?: CompareSeed | null };
	/** the enlarged face-check viewer's photo + faces */
	check: { view: CheckView };
	/** the camera capture: the taken File is handed to the opener, then the
	 *  modal closes itself */
	camera: { onCapture: (file: File) => void };
	/** the stage photo editor: source file, the export callback, and the
	 *  per-file session vault (initialSession in, onSaveSession out at close) */
	edit: {
		source: File;
		onUse: (file: File) => void;
		initialSession?: EditSession | null;
		onSaveSession?: (source: File, session: EditSession) => void;
	};
	repos: Record<string, never>;
	users: Record<string, never>;
	keys: Record<string, never>;
	password: Record<string, never>;
}

/** The modal components, by key. */
export const MODAL_DEFS: Record<ModalKey, ComponentType<any>> = {
	enroll: EnrollModal,
	person: PersonModal,
	photos: PhotosModal,
	compare: CompareModal,
	check: FaceCheckModal,
	edit: EditModal,
	camera: CameraModal,
	repos: ReposModal,
	users: UsersModal,
	keys: ApiKeysModal,
	password: ChangePasswordModal,
};

export interface TypedModalService {
	open: <K extends ModalKey>(key: K, args?: ModalArgs[K]) => void;
	close: (...keys: ModalKey[]) => void;
	patch: <K extends ModalKey>(
		key: K,
		partial: Partial<ModalArgs[K]>,
		when?: (current: ModalArgs[K] | null) => boolean,
	) => void;
}

/** The typed modal facade. The generic pool service underneath is
 *  string-keyed; this wrapper keeps every call site checked. */
export function useModals(): TypedModalService {
	return useModalService() as unknown as TypedModalService;
}
