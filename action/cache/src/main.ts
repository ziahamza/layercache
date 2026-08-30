import * as cache from "@actions/cache";
import * as core from "@actions/core";

import { runRestore } from "./action.js";

void runRestore({ core, cache, env: process.env });
