// Camera capture modal.
// Live getUserMedia preview with a camera picker; the shutter captures the
// current frame as a JPEG File and hands it to the opener (which routes it
// through its own ingestion flow), then the modal closes. One capture per
// open — retaking means reopening (permission persists, so it is fast).
//
// getUserMedia exists only in a secure context (HTTPS or localhost); the
// opener buttons hide on `cameraAvailable`, and the modal still guards the
// case with a message. Stream lifecycle: every stream is stopped on close or
// unmount and before each restart; superseded async starts are discarded by a
// sequence guard — no camera can stay hot after the modal is gone.

import { useLayoutEffect, useRef } from "preact/hooks";
import { useSignal } from "@preact/signals";
import { Show } from "@preact/signals/utils";
import { Modal } from "./Modal";

interface CameraModalProps {
	/** injected by the modal service (instances render open) */
	open: boolean;
	/** injected by the modal service; closing unmounts the instance */
	onCloseRequest: () => void;
	/** the captured File is handed to the opener, then the modal closes */
	onCapture: (file: File) => void;
}

function errorText(e: DOMException | null): string {
	switch (e?.name) {
		case "NotAllowedError":
		case "SecurityError":
			return "Camera access was denied — allow it in the browser's site settings, then retry.";
		case "NotFoundError":
		case "OverconstrainedError":
			return "No usable camera was found on this device.";
		case "NotReadableError":
		case "AbortError":
			return "The camera is busy — close other apps using it and retry.";
		default:
			return `Could not start the camera${e?.message ? ` (${e.message})` : ""}.`;
	}
}

export function CameraModal({ open, onCloseRequest, onCapture }: CameraModalProps) {
	const status = useSignal<"starting" | "live" | "error">("starting");
	const errText = useSignal("");
	const errIsUnsupported = useSignal(false);
	const devices = useSignal<MediaDeviceInfo[]>([]);
	const deviceId = useSignal("");
	const capturing = useSignal(false);
	// Front cameras (and desktop webcams, which report no facingMode) are
	// previewed mirrored; the capture applies the same flip so the file
	// matches what the user framed. Rear cameras stay unmirrored.
	const mirror = useSignal(false);

	const videoRef = useRef<HTMLVideoElement | null>(null);
	const streamRef = useRef<MediaStream | null>(null);
	const shutterRef = useRef<HTMLButtonElement | null>(null);
	// Sequence guard: a flip's getUserMedia resolving after a newer flip's
	// (or after close) must be stopped, not adopted.
	const seqRef = useRef(0);

	const stopStream = () => {
		streamRef.current?.getTracks().forEach((t) => { t.onended = null; t.stop(); });
		streamRef.current = null;
	};

	const fail = (msg: string, unsupported = false) => {
		stopStream();
		status.value = "error";
		errText.value = msg;
		errIsUnsupported.value = unsupported;
	};

	// Start (or restart on a camera switch): one live stream at a time.
	const start = async (wantId?: string) => {
		const seq = ++seqRef.current;
		const md = navigator.mediaDevices;
		if (!md?.getUserMedia) {
			fail("Camera capture needs a secure context — open this page over HTTPS or localhost.", true);
			return;
		}
		stopStream();
		status.value = "starting";
		errText.value = "";
		try {
			const stream = await md.getUserMedia({
				video: wantId
					? { deviceId: { exact: wantId }, width: { ideal: 1280 }, height: { ideal: 720 } }
					: { facingMode: "user", width: { ideal: 1280 }, height: { ideal: 720 } },
				audio: false,
			});
			if (seq !== seqRef.current) { // a newer start or close won
				stream.getTracks().forEach((t) => t.stop());
				return;
			}
			const track = stream.getVideoTracks()[0];
			track.addEventListener("ended", () => {
				if (seq === seqRef.current) fail("The camera stream ended.");
			});
			streamRef.current = stream;
			const s = track.getSettings();
			mirror.value = s.facingMode !== "environment";
			deviceId.value = s.deviceId ?? wantId ?? "";
			if (videoRef.current) videoRef.current.srcObject = stream;
			status.value = "live";
			// Labels are only populated once permission is granted; the picker
			// stays hidden while there is nothing to choose between.
			try {
				const cams = (await md.enumerateDevices()).filter((d) => d.kind === "videoinput");
				if (seq === seqRef.current) devices.value = cams;
			} catch { /* the picker is best-effort */ }
		} catch (e) {
			if (seq === seqRef.current) fail(errorText(e as DOMException));
		}
	};

	// Open → start; close or unmount → always release the camera.
	useLayoutEffect(() => {
		if (!open) return;
		void start();
		return () => { seqRef.current++; stopStream(); };
	}, [open]);

	const capture = () => {
		const v = videoRef.current;
		if (capturing.value || status.value !== "live" || !v || !v.videoWidth) return;
		const canvas = document.createElement("canvas");
		canvas.width = v.videoWidth;
		canvas.height = v.videoHeight;
		const ctx = canvas.getContext("2d");
		if (!ctx) { fail("Capture failed — the frame could not be drawn."); return; }
		if (mirror.value) { ctx.translate(canvas.width, 0); ctx.scale(-1, 1); }
		ctx.drawImage(v, 0, 0, canvas.width, canvas.height);
		const seq = seqRef.current;
		capturing.value = true;
		try {
			canvas.toBlob((blob) => {
				capturing.value = false;
				if (seq !== seqRef.current) return;
				if (!blob) { fail("Capture failed — the frame could not be encoded."); return; }
				stopStream();
				onCapture(new File([blob], `camera-${Date.now()}.jpg`, { type: "image/jpeg" }));
				onCloseRequest();
			}, "image/jpeg", 0.92);
		} catch (e) {
			capturing.value = false;
			if (e instanceof DOMException && e.name === "SecurityError") {
				fail("Capture failed — the frame could not be encoded.");
				return;
			}
			throw e;
		}
	};

	const live = status.value === "live";

	return (
		<Modal
			open={open}
			id="cameraModal"
			cardId="cameraCard"
			eyebrow="camera"
			titleId="cameraTitle"
			title="Take a photo"
			initialFocus={() => shutterRef.current}
			onCloseRequest={onCloseRequest}
			body={
				<div class="modal-body camera-body">
					<div class={"camera-stage" + (mirror.value ? " mirror" : "")} id="cameraStage">
						<video ref={videoRef} autoplay playsinline muted />
						<Show when={() => status.value !== "live"} fallback={null}>
							{() => (
								<div class="camera-overlay" id="cameraOverlay" aria-live="polite">
									<span>
										{status.value === "starting" ? "Starting the camera…" : errText.value}
									</span>
									{status.value === "error" && !errIsUnsupported.value && (
										<button type="button" class="btn btn-sm" onClick={() => { void start(deviceId.value || undefined); }}>
											Retry
										</button>
									)}
								</div>
							)}
						</Show>
					</div>
					<Show when={() => devices.value.length > 1} fallback={null}>
						{() => (
							<select
								id="cameraSelect"
								class="camera-select"
								aria-label="Camera to use"
								disabled={!live}
								value={deviceId}
								onChange={(e) => { void start(e.currentTarget.value); }}
							>
								{devices.value.map((d) => (
									<option key={d.deviceId} value={d.deviceId}>{d.label || "Camera"}</option>
								))}
							</select>
						)}
					</Show>
					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close>Cancel</button>
						<button
							ref={shutterRef}
							type="button"
							id="cameraShutter"
							class="btn btn-accent"
							disabled={!live || capturing.value}
							onClick={capture}
						>
							Take photo
						</button>
					</footer>
				</div>
			}
		/>
	);
}
