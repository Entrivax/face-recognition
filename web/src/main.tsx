// recogn — UI entry point.

import { render } from "preact";
import { ToastProvider } from "./toast";
import { App } from "./components/App";
import "./style.css";

render(
	<ToastProvider>
		<App />
	</ToastProvider>,
	document.getElementById("root")!
);
