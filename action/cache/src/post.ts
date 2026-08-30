import * as cache from "@actions/cache";
import * as core from "@actions/core";

import { runSave } from "./action.js";

void runSave({ core, cache, env: process.env });
