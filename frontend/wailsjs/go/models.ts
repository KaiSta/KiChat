export namespace chat {
	
	export class CompactResult {
	    created: boolean;
	    compaction: store.Compaction;
	
	    static createFrom(source: any = {}) {
	        return new CompactResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.created = source["created"];
	        this.compaction = this.convertValues(source["compaction"], store.Compaction);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ContextSegment {
	    kind: string;
	    label: string;
	    tokens: number;
	    detail?: string;
	
	    static createFrom(source: any = {}) {
	        return new ContextSegment(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.label = source["label"];
	        this.tokens = source["tokens"];
	        this.detail = source["detail"];
	    }
	}
	export class ContextBudget {
	    limit: number;
	    limitSource: string;
	    ceiling: number;
	    ceilingSource: string;
	    effectiveLimit: number;
	    system: number;
	    memory: number;
	    attachments: number;
	    history: number;
	    toolSchemas: number;
	    reserve: number;
	    estimated: number;
	    measured: number;
	    measuredValid: boolean;
	    measuredSeq: number;
	    deltaSince: number;
	    used: number;
	    percent: number;
	    projected: number;
	    projectedRatio: number;
	    overLimit: boolean;
	    segments: ContextSegment[];
	
	    static createFrom(source: any = {}) {
	        return new ContextBudget(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.limit = source["limit"];
	        this.limitSource = source["limitSource"];
	        this.ceiling = source["ceiling"];
	        this.ceilingSource = source["ceilingSource"];
	        this.effectiveLimit = source["effectiveLimit"];
	        this.system = source["system"];
	        this.memory = source["memory"];
	        this.attachments = source["attachments"];
	        this.history = source["history"];
	        this.toolSchemas = source["toolSchemas"];
	        this.reserve = source["reserve"];
	        this.estimated = source["estimated"];
	        this.measured = source["measured"];
	        this.measuredValid = source["measuredValid"];
	        this.measuredSeq = source["measuredSeq"];
	        this.deltaSince = source["deltaSince"];
	        this.used = source["used"];
	        this.percent = source["percent"];
	        this.projected = source["projected"];
	        this.projectedRatio = source["projectedRatio"];
	        this.overLimit = source["overLimit"];
	        this.segments = this.convertValues(source["segments"], ContextSegment);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace config {
	
	export class AutoApproveSettings {
	    files: boolean;
	    commands: boolean;
	
	    static createFrom(source: any = {}) {
	        return new AutoApproveSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.files = source["files"];
	        this.commands = source["commands"];
	    }
	}
	export class CompactSettings {
	    enabled: boolean;
	    midTurnCheck: boolean;
	    threshold: number;
	    keepRecentTurns: number;
	    reserveForResponse: number;
	    summaryTemperature: number;
	    maxSummaryTokens: number;
	    summaryModel: llm.Ref;
	
	    static createFrom(source: any = {}) {
	        return new CompactSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.midTurnCheck = source["midTurnCheck"];
	        this.threshold = source["threshold"];
	        this.keepRecentTurns = source["keepRecentTurns"];
	        this.reserveForResponse = source["reserveForResponse"];
	        this.summaryTemperature = source["summaryTemperature"];
	        this.maxSummaryTokens = source["maxSummaryTokens"];
	        this.summaryModel = this.convertValues(source["summaryModel"], llm.Ref);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class DigestSettings {
	    model: llm.Ref;
	    threshold: number;
	    maxTokens: number;
	
	    static createFrom(source: any = {}) {
	        return new DigestSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.model = this.convertValues(source["model"], llm.Ref);
	        this.threshold = source["threshold"];
	        this.maxTokens = source["maxTokens"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ExpertChoice {
	    offload: string;
	    layers: number;
	
	    static createFrom(source: any = {}) {
	        return new ExpertChoice(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.offload = source["offload"];
	        this.layers = source["layers"];
	    }
	}
	export class ImageSettings {
	    enabled: boolean;
	    baseUrl: string;
	    apiKey: string;
	    model: string;
	    sampler: string;
	    steps: number;
	    width: number;
	    height: number;
	    negativePrompt: string;
	    cfgScale: number;
	    captioning: string;
	    pipeline: string;
	    hiresEnabled: boolean;
	    hiresScale: number;
	    hiresDenoise: number;
	    saveFolder: string;
	    upscalerEnabled: boolean;
	    upscaleDefaultMethod: string;
	    upscaleSharpen: number;
	    esrganBinary: string;
	    esrganModel: string;
	    esrganTile: number;
	    kind: string;
	    nimModel: string;
	    startWithApp: boolean;
	    cliBinary: string;
	    cliFolder: string;
	    cliModel: string;
	    cliDiffusionModel: string;
	    cliVae: string;
	    cliLlm: string;
	    cliClipL: string;
	    cliClipG: string;
	    cliT5xxl: string;
	    cliOffloadToCpu: boolean;
	    cliExtraArgs: string;
	    cliVaeTiling: boolean;
	    cliVaeTileSize: number;
	
	    static createFrom(source: any = {}) {
	        return new ImageSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.baseUrl = source["baseUrl"];
	        this.apiKey = source["apiKey"];
	        this.model = source["model"];
	        this.sampler = source["sampler"];
	        this.steps = source["steps"];
	        this.width = source["width"];
	        this.height = source["height"];
	        this.negativePrompt = source["negativePrompt"];
	        this.cfgScale = source["cfgScale"];
	        this.captioning = source["captioning"];
	        this.pipeline = source["pipeline"];
	        this.hiresEnabled = source["hiresEnabled"];
	        this.hiresScale = source["hiresScale"];
	        this.hiresDenoise = source["hiresDenoise"];
	        this.saveFolder = source["saveFolder"];
	        this.upscalerEnabled = source["upscalerEnabled"];
	        this.upscaleDefaultMethod = source["upscaleDefaultMethod"];
	        this.upscaleSharpen = source["upscaleSharpen"];
	        this.esrganBinary = source["esrganBinary"];
	        this.esrganModel = source["esrganModel"];
	        this.esrganTile = source["esrganTile"];
	        this.kind = source["kind"];
	        this.nimModel = source["nimModel"];
	        this.startWithApp = source["startWithApp"];
	        this.cliBinary = source["cliBinary"];
	        this.cliFolder = source["cliFolder"];
	        this.cliModel = source["cliModel"];
	        this.cliDiffusionModel = source["cliDiffusionModel"];
	        this.cliVae = source["cliVae"];
	        this.cliLlm = source["cliLlm"];
	        this.cliClipL = source["cliClipL"];
	        this.cliClipG = source["cliClipG"];
	        this.cliT5xxl = source["cliT5xxl"];
	        this.cliOffloadToCpu = source["cliOffloadToCpu"];
	        this.cliExtraArgs = source["cliExtraArgs"];
	        this.cliVaeTiling = source["cliVaeTiling"];
	        this.cliVaeTileSize = source["cliVaeTileSize"];
	    }
	}
	export class ImageProfile {
	    name: string;
	    settings: ImageSettings;
	
	    static createFrom(source: any = {}) {
	        return new ImageProfile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.settings = this.convertValues(source["settings"], ImageSettings);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class MLXOptions {
	    kvBits?: number;
	    prefillStep?: number;
	    draftModel?: string;
	    maxTokens?: number;
	    chatTemplate?: string;
	    trustRemoteCode?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new MLXOptions(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kvBits = source["kvBits"];
	        this.prefillStep = source["prefillStep"];
	        this.draftModel = source["draftModel"];
	        this.maxTokens = source["maxTokens"];
	        this.chatTemplate = source["chatTemplate"];
	        this.trustRemoteCode = source["trustRemoteCode"];
	    }
	}
	export class MLXRuntime {
	    version: string;
	    serverPath: string;
	
	    static createFrom(source: any = {}) {
	        return new MLXRuntime(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.serverPath = source["serverPath"];
	    }
	}
	export class ManagedModel {
	    enabled: boolean;
	    modelPath: string;
	    mmproj: string;
	    ctxSize: number;
	    flashAttention: string;
	    cacheTypeK: string;
	    cacheTypeV: string;
	    offload: string;
	    expertOffload: string;
	    expertLayers: number;
	    experts?: Record<string, ExpertChoice>;
	    mtp?: Record<string, number>;
	    mtpConfidence?: Record<string, number>;
	    extraArgs: string;
	    parallel: boolean;
	    mlx?: MLXOptions;
	
	    static createFrom(source: any = {}) {
	        return new ManagedModel(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.modelPath = source["modelPath"];
	        this.mmproj = source["mmproj"];
	        this.ctxSize = source["ctxSize"];
	        this.flashAttention = source["flashAttention"];
	        this.cacheTypeK = source["cacheTypeK"];
	        this.cacheTypeV = source["cacheTypeV"];
	        this.offload = source["offload"];
	        this.expertOffload = source["expertOffload"];
	        this.expertLayers = source["expertLayers"];
	        this.experts = this.convertValues(source["experts"], ExpertChoice, true);
	        this.mtp = source["mtp"];
	        this.mtpConfidence = source["mtpConfidence"];
	        this.extraArgs = source["extraArgs"];
	        this.parallel = source["parallel"];
	        this.mlx = this.convertValues(source["mlx"], MLXOptions);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PromptPreset {
	    id: string;
	    name: string;
	    prompt: string;
	    builtIn: boolean;
	
	    static createFrom(source: any = {}) {
	        return new PromptPreset(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.prompt = source["prompt"];
	        this.builtIn = source["builtIn"];
	    }
	}
	export class RuntimeSettings {
	    distribution: string;
	    tag: string;
	    binaryPath: string;
	    accelerator: string;
	    readySeconds: number;
	    modelDir: string;
	    modelDirs: string[];
	    mlx: MLXRuntime;
	    models: Record<string, ManagedModel>;
	
	    static createFrom(source: any = {}) {
	        return new RuntimeSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.distribution = source["distribution"];
	        this.tag = source["tag"];
	        this.binaryPath = source["binaryPath"];
	        this.accelerator = source["accelerator"];
	        this.readySeconds = source["readySeconds"];
	        this.modelDir = source["modelDir"];
	        this.modelDirs = source["modelDirs"];
	        this.mlx = this.convertValues(source["mlx"], MLXRuntime);
	        this.models = this.convertValues(source["models"], ManagedModel, true);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SamplerSettings {
	    temperature: number;
	    topP: number;
	    maxTokens: number;
	    numCtx: number;
	
	    static createFrom(source: any = {}) {
	        return new SamplerSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.temperature = source["temperature"];
	        this.topP = source["topP"];
	        this.maxTokens = source["maxTokens"];
	        this.numCtx = source["numCtx"];
	    }
	}
	export class SearchSettings {
	    providerOrder: string[];
	    braveApiKey: string;
	    country: string;
	    searchLang: string;
	    resultCount: number;
	    freshness: string;
	    extraSnippets: boolean;
	    cacheMinutes: number;
	    useLlmContext: boolean;
	    llmContextTokens: number;
	    scrapeRespectRobots: boolean;
	    scrapeMaxBytes: number;
	    scrapeTimeoutSeconds: number;
	    scrapeAllowPrivateHosts: boolean;
	    scrapeCacheHours: number;
	
	    static createFrom(source: any = {}) {
	        return new SearchSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.providerOrder = source["providerOrder"];
	        this.braveApiKey = source["braveApiKey"];
	        this.country = source["country"];
	        this.searchLang = source["searchLang"];
	        this.resultCount = source["resultCount"];
	        this.freshness = source["freshness"];
	        this.extraSnippets = source["extraSnippets"];
	        this.cacheMinutes = source["cacheMinutes"];
	        this.useLlmContext = source["useLlmContext"];
	        this.llmContextTokens = source["llmContextTokens"];
	        this.scrapeRespectRobots = source["scrapeRespectRobots"];
	        this.scrapeMaxBytes = source["scrapeMaxBytes"];
	        this.scrapeTimeoutSeconds = source["scrapeTimeoutSeconds"];
	        this.scrapeAllowPrivateHosts = source["scrapeAllowPrivateHosts"];
	        this.scrapeCacheHours = source["scrapeCacheHours"];
	    }
	}
	export class UpdateSettings {
	    checkDaily: boolean;
	    lastCheckUnix?: number;
	
	    static createFrom(source: any = {}) {
	        return new UpdateSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.checkDaily = source["checkDaily"];
	        this.lastCheckUnix = source["lastCheckUnix"];
	    }
	}
	export class Settings {
	    language: string;
	    location: string;
	    theme: string;
	    backends: llm.BackendConfig[];
	    modelOverrides: Record<string, llm.ModelOverride>;
	    search: SearchSettings;
	    compact: CompactSettings;
	    digest: DigestSettings;
	    sampler: SamplerSettings;
	    defaultModel: llm.Ref;
	    defaultSystemPrompt: string;
	    promptPresets: PromptPreset[];
	    chatPromptPreset: string;
	    codingPromptPreset: string;
	    autoTitle: boolean;
	    toolIterations: number;
	    codingToolIterations: number;
	    nativeTools: boolean;
	    runtime: RuntimeSettings;
	    hiddenModels?: string[];
	    images: ImageSettings;
	    imageProfiles?: ImageProfile[];
	    autoApprove: AutoApproveSettings;
	    updates: UpdateSettings;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.language = source["language"];
	        this.location = source["location"];
	        this.theme = source["theme"];
	        this.backends = this.convertValues(source["backends"], llm.BackendConfig);
	        this.modelOverrides = this.convertValues(source["modelOverrides"], llm.ModelOverride, true);
	        this.search = this.convertValues(source["search"], SearchSettings);
	        this.compact = this.convertValues(source["compact"], CompactSettings);
	        this.digest = this.convertValues(source["digest"], DigestSettings);
	        this.sampler = this.convertValues(source["sampler"], SamplerSettings);
	        this.defaultModel = this.convertValues(source["defaultModel"], llm.Ref);
	        this.defaultSystemPrompt = source["defaultSystemPrompt"];
	        this.promptPresets = this.convertValues(source["promptPresets"], PromptPreset);
	        this.chatPromptPreset = source["chatPromptPreset"];
	        this.codingPromptPreset = source["codingPromptPreset"];
	        this.autoTitle = source["autoTitle"];
	        this.toolIterations = source["toolIterations"];
	        this.codingToolIterations = source["codingToolIterations"];
	        this.nativeTools = source["nativeTools"];
	        this.runtime = this.convertValues(source["runtime"], RuntimeSettings);
	        this.hiddenModels = source["hiddenModels"];
	        this.images = this.convertValues(source["images"], ImageSettings);
	        this.imageProfiles = this.convertValues(source["imageProfiles"], ImageProfile);
	        this.autoApprove = this.convertValues(source["autoApprove"], AutoApproveSettings);
	        this.updates = this.convertValues(source["updates"], UpdateSettings);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace images {
	
	export class SDBuild {
	    id: string;
	    accelerator: string;
	    asset: string;
	    noteKey?: string;
	
	    static createFrom(source: any = {}) {
	        return new SDBuild(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.accelerator = source["accelerator"];
	        this.asset = source["asset"];
	        this.noteKey = source["noteKey"];
	    }
	}
	export class UpscaleRequest {
	    target: string;
	    longSide: number;
	    method: string;
	    model: string;
	    sharpen: number;
	
	    static createFrom(source: any = {}) {
	        return new UpscaleRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.target = source["target"];
	        this.longSide = source["longSide"];
	        this.method = source["method"];
	        this.model = source["model"];
	        this.sharpen = source["sharpen"];
	    }
	}

}

export namespace llm {
	
	export class BackendConfig {
	    id: string;
	    name: string;
	    kind: string;
	    baseUrl: string;
	    apiKey: string;
	    enabled: boolean;
	    digest: boolean;
	
	    static createFrom(source: any = {}) {
	        return new BackendConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.kind = source["kind"];
	        this.baseUrl = source["baseUrl"];
	        this.apiKey = source["apiKey"];
	        this.enabled = source["enabled"];
	        this.digest = source["digest"];
	    }
	}
	export class HealthStatus {
	    id: string;
	    name: string;
	    kind: string;
	    baseUrl: string;
	    enabled: boolean;
	    ok: boolean;
	    error?: string;
	    models: number;
	
	    static createFrom(source: any = {}) {
	        return new HealthStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.kind = source["kind"];
	        this.baseUrl = source["baseUrl"];
	        this.enabled = source["enabled"];
	        this.ok = source["ok"];
	        this.error = source["error"];
	        this.models = source["models"];
	    }
	}
	export class ModelInfo {
	    id: string;
	    backend: string;
	    publisher?: string;
	    arch?: string;
	    quantization?: string;
	    state?: string;
	    type?: string;
	    contextLimit: number;
	    maxContextLimit?: number;
	    capabilities?: string[];
	    parameterCount?: string;
	
	    static createFrom(source: any = {}) {
	        return new ModelInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.backend = source["backend"];
	        this.publisher = source["publisher"];
	        this.arch = source["arch"];
	        this.quantization = source["quantization"];
	        this.state = source["state"];
	        this.type = source["type"];
	        this.contextLimit = source["contextLimit"];
	        this.maxContextLimit = source["maxContextLimit"];
	        this.capabilities = source["capabilities"];
	        this.parameterCount = source["parameterCount"];
	    }
	}
	export class ModelOverride {
	    contextLimit?: number;
	    capabilities?: string[];
	    requestCeiling?: number;
	
	    static createFrom(source: any = {}) {
	        return new ModelOverride(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.contextLimit = source["contextLimit"];
	        this.capabilities = source["capabilities"];
	        this.requestCeiling = source["requestCeiling"];
	    }
	}
	export class Ref {
	    backend: string;
	    model: string;
	
	    static createFrom(source: any = {}) {
	        return new Ref(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.backend = source["backend"];
	        this.model = source["model"];
	    }
	}

}

export namespace main {
	
	export class AppInfo {
	    version: string;
	    commit: string;
	    date: string;
	    goVersion: string;
	    platform: string;
	
	    static createFrom(source: any = {}) {
	        return new AppInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.commit = source["commit"];
	        this.date = source["date"];
	        this.goVersion = source["goVersion"];
	        this.platform = source["platform"];
	    }
	}
	export class AppUpdate {
	    checked: boolean;
	    available: boolean;
	    current?: string;
	    latest?: string;
	    url?: string;
	    publishedAt?: string;
	    notes?: string;
	    devBuild?: boolean;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new AppUpdate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.checked = source["checked"];
	        this.available = source["available"];
	        this.current = source["current"];
	        this.latest = source["latest"];
	        this.url = source["url"];
	        this.publishedAt = source["publishedAt"];
	        this.notes = source["notes"];
	        this.devBuild = source["devBuild"];
	        this.error = source["error"];
	    }
	}
	export class AttachmentInput {
	    chatId: string;
	    path: string;
	    placement: string;
	
	    static createFrom(source: any = {}) {
	        return new AttachmentInput(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.chatId = source["chatId"];
	        this.path = source["path"];
	        this.placement = source["placement"];
	    }
	}
	export class BackupEntry {
	    chatId: string;
	    title: string;
	    // Go type: time
	    updatedAt: any;
	    messages: number;
	    attachments: number;
	    bytes: number;
	    state: string;
	
	    static createFrom(source: any = {}) {
	        return new BackupEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.chatId = source["chatId"];
	        this.title = source["title"];
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.messages = source["messages"];
	        this.attachments = source["attachments"];
	        this.bytes = source["bytes"];
	        this.state = source["state"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class BackupExportRequest {
	    chatIds: string[];
	    passphrase: string;
	    title: string;
	
	    static createFrom(source: any = {}) {
	        return new BackupExportRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.chatIds = source["chatIds"];
	        this.passphrase = source["passphrase"];
	        this.title = source["title"];
	    }
	}
	export class BackupExportResult {
	    path: string;
	    chats: number;
	    bytes: number;
	
	    static createFrom(source: any = {}) {
	        return new BackupExportResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.chats = source["chats"];
	        this.bytes = source["bytes"];
	    }
	}
	export class BackupImportResult {
	    imported: string[];
	    replaced: string[];
	    skipped: string[];
	
	    static createFrom(source: any = {}) {
	        return new BackupImportResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.imported = source["imported"];
	        this.replaced = source["replaced"];
	        this.skipped = source["skipped"];
	    }
	}
	export class BackupPreview {
	    path: string;
	    app: string;
	    // Go type: time
	    exported: any;
	    chats: BackupEntry[];
	
	    static createFrom(source: any = {}) {
	        return new BackupPreview(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.app = source["app"];
	        this.exported = this.convertValues(source["exported"], null);
	        this.chats = this.convertValues(source["chats"], BackupEntry);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ChatConsent {
	    files: boolean;
	    commands: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ChatConsent(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.files = source["files"];
	        this.commands = source["commands"];
	    }
	}
	export class ChatState {
	    chat: store.Chat;
	    messages: store.Message[];
	    compactions: store.Compaction[];
	    attachments: store.Attachment[];
	    documents: store.Document[];
	    budget: chat.ContextBudget;
	    running: boolean;
	    consent: ChatConsent;
	
	    static createFrom(source: any = {}) {
	        return new ChatState(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.chat = this.convertValues(source["chat"], store.Chat);
	        this.messages = this.convertValues(source["messages"], store.Message);
	        this.compactions = this.convertValues(source["compactions"], store.Compaction);
	        this.attachments = this.convertValues(source["attachments"], store.Attachment);
	        this.documents = this.convertValues(source["documents"], store.Document);
	        this.budget = this.convertValues(source["budget"], chat.ContextBudget);
	        this.running = source["running"];
	        this.consent = this.convertValues(source["consent"], ChatConsent);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class CompactRequest {
	    chatId: string;
	    focus: string;
	    target: number;
	    fromSeq: number;
	    toSeq: number;
	
	    static createFrom(source: any = {}) {
	        return new CompactRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.chatId = source["chatId"];
	        this.focus = source["focus"];
	        this.target = source["target"];
	        this.fromSeq = source["fromSeq"];
	        this.toSeq = source["toSeq"];
	    }
	}
	export class DatabaseStatus {
	    locked: boolean;
	    setup: boolean;
	    encrypted: boolean;
	    recovery: boolean;
	    keyring: boolean;
	    remembered: boolean;
	    reason: string;
	
	    static createFrom(source: any = {}) {
	        return new DatabaseStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.locked = source["locked"];
	        this.setup = source["setup"];
	        this.encrypted = source["encrypted"];
	        this.recovery = source["recovery"];
	        this.keyring = source["keyring"];
	        this.remembered = source["remembered"];
	        this.reason = source["reason"];
	    }
	}
	export class FileChange {
	    path: string;
	    operation: string;
	    revisions: number;
	    hunks: workspace.Hunk[];
	    added: number;
	    removed: number;
	    truncated: boolean;
	    newlineOnly: boolean;
	    canRevert: boolean;
	    blocked: string;
	    // Go type: time
	    changedAt: any;
	
	    static createFrom(source: any = {}) {
	        return new FileChange(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.operation = source["operation"];
	        this.revisions = source["revisions"];
	        this.hunks = this.convertValues(source["hunks"], workspace.Hunk);
	        this.added = source["added"];
	        this.removed = source["removed"];
	        this.truncated = source["truncated"];
	        this.newlineOnly = source["newlineOnly"];
	        this.canRevert = source["canRevert"];
	        this.blocked = source["blocked"];
	        this.changedAt = this.convertValues(source["changedAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ImageCache {
	    count: number;
	    bytes: number;
	    unsavedCount: number;
	    unsavedBytes: number;
	    unsavedChats: number;
	    folder: string;
	
	    static createFrom(source: any = {}) {
	        return new ImageCache(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.count = source["count"];
	        this.bytes = source["bytes"];
	        this.unsavedCount = source["unsavedCount"];
	        this.unsavedBytes = source["unsavedBytes"];
	        this.unsavedChats = source["unsavedChats"];
	        this.folder = source["folder"];
	    }
	}
	export class ImageCacheClear {
	    removed: number;
	    bytes: number;
	
	    static createFrom(source: any = {}) {
	        return new ImageCacheClear(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.removed = source["removed"];
	        this.bytes = source["bytes"];
	    }
	}
	export class ImageServerStatus {
	    state: string;
	    url: string;
	    port: number;
	    pid: number;
	    error?: string;
	    log: string[];
	    restartNeeded: boolean;
	    startWithApp: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ImageServerStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.state = source["state"];
	        this.url = source["url"];
	        this.port = source["port"];
	        this.pid = source["pid"];
	        this.error = source["error"];
	        this.log = source["log"];
	        this.restartNeeded = source["restartNeeded"];
	        this.startWithApp = source["startWithApp"];
	    }
	}
	export class ImageStatus {
	    enabled: boolean;
	    baseURL: string;
	    ok: boolean;
	    error?: string;
	    models: string[];
	    samplers: string[];
	    keySet: boolean;
	    kind: string;
	    binary: string;
	    folder: string;
	    build: string;
	    builds: images.SDBuild[];
	    releasePage: string;
	    files: string[];
	    program: string;
	    pipeline: string;
	    pipelineName: string;
	    pipelineSource: string;
	    captioning: string;
	    server: ImageServerStatus;
	
	    static createFrom(source: any = {}) {
	        return new ImageStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.baseURL = source["baseURL"];
	        this.ok = source["ok"];
	        this.error = source["error"];
	        this.models = source["models"];
	        this.samplers = source["samplers"];
	        this.keySet = source["keySet"];
	        this.kind = source["kind"];
	        this.binary = source["binary"];
	        this.folder = source["folder"];
	        this.build = source["build"];
	        this.builds = this.convertValues(source["builds"], images.SDBuild);
	        this.releasePage = source["releasePage"];
	        this.files = source["files"];
	        this.program = source["program"];
	        this.pipeline = source["pipeline"];
	        this.pipelineName = source["pipelineName"];
	        this.pipelineSource = source["pipelineSource"];
	        this.captioning = source["captioning"];
	        this.server = this.convertValues(source["server"], ImageServerStatus);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class InstalledBuild {
	    dir: string;
	    size: number;
	    // Go type: time
	    installedAt: any;
	    inUse: boolean;
	    running: boolean;
	    incomplete: boolean;
	
	    static createFrom(source: any = {}) {
	        return new InstalledBuild(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.dir = source["dir"];
	        this.size = source["size"];
	        this.installedAt = this.convertValues(source["installedAt"], null);
	        this.inUse = source["inUse"];
	        this.running = source["running"];
	        this.incomplete = source["incomplete"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ManagedModelTraits {
	    architecture?: string;
	    blockCount: number;
	    expertCount: number;
	    expertUsedCount: number;
	    mtpLayers: number;
	
	    static createFrom(source: any = {}) {
	        return new ManagedModelTraits(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.architecture = source["architecture"];
	        this.blockCount = source["blockCount"];
	        this.expertCount = source["expertCount"];
	        this.expertUsedCount = source["expertUsedCount"];
	        this.mtpLayers = source["mtpLayers"];
	    }
	}
	export class ManagedModelProbe {
	    traits: ManagedModelTraits;
	    suggestedLayers: number;
	    suggestedMtpTokens: number;
	    suggestedMtpConfidence: number;
	
	    static createFrom(source: any = {}) {
	        return new ManagedModelProbe(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.traits = this.convertValues(source["traits"], ManagedModelTraits);
	        this.suggestedLayers = source["suggestedLayers"];
	        this.suggestedMtpTokens = source["suggestedMtpTokens"];
	        this.suggestedMtpConfidence = source["suggestedMtpConfidence"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class ManagedStatus {
	    backendId: string;
	    kind?: string;
	    enabled: boolean;
	    modelPath: string;
	    loadedModel?: string;
	    ctxSize: number;
	    extraArgs: string;
	    mmproj?: string;
	    mmprojDetected?: boolean;
	    state: string;
	    port: number;
	    url: string;
	    error?: string;
	    log?: string[];
	    modelBytes: number;
	    offloadLayers: number;
	    offloadTotal: number;
	    offloadDevice?: string;
	
	    static createFrom(source: any = {}) {
	        return new ManagedStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.backendId = source["backendId"];
	        this.kind = source["kind"];
	        this.enabled = source["enabled"];
	        this.modelPath = source["modelPath"];
	        this.loadedModel = source["loadedModel"];
	        this.ctxSize = source["ctxSize"];
	        this.extraArgs = source["extraArgs"];
	        this.mmproj = source["mmproj"];
	        this.mmprojDetected = source["mmprojDetected"];
	        this.state = source["state"];
	        this.port = source["port"];
	        this.url = source["url"];
	        this.error = source["error"];
	        this.log = source["log"];
	        this.modelBytes = source["modelBytes"];
	        this.offloadLayers = source["offloadLayers"];
	        this.offloadTotal = source["offloadTotal"];
	        this.offloadDevice = source["offloadDevice"];
	    }
	}
	export class ModelCheckStatus {
	    backend?: string;
	    busy: boolean;
	    done: number;
	    total: number;
	    hidden: number;
	    hiddenTotal: number;
	    error?: string;
	    reason?: string;
	
	    static createFrom(source: any = {}) {
	        return new ModelCheckStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.backend = source["backend"];
	        this.busy = source["busy"];
	        this.done = source["done"];
	        this.total = source["total"];
	        this.hidden = source["hidden"];
	        this.hiddenTotal = source["hiddenTotal"];
	        this.error = source["error"];
	        this.reason = source["reason"];
	    }
	}
	export class PromptVariable {
	    name: string;
	    key: string;
	    description: string;
	
	    static createFrom(source: any = {}) {
	        return new PromptVariable(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.key = source["key"];
	        this.description = source["description"];
	    }
	}
	export class RuntimeUpdate {
	    checked: boolean;
	    available: boolean;
	    family?: string;
	    distribution?: string;
	    installed?: string;
	    latest?: string;
	    size: number;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new RuntimeUpdate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.checked = source["checked"];
	        this.available = source["available"];
	        this.family = source["family"];
	        this.distribution = source["distribution"];
	        this.installed = source["installed"];
	        this.latest = source["latest"];
	        this.size = source["size"];
	        this.error = source["error"];
	    }
	}
	export class RuntimeStatus {
	    installed: boolean;
	    tag?: string;
	    binary?: string;
	    version?: string;
	    customBinary?: string;
	    devices: string[];
	    devicesError?: string;
	    devicesMissingLibs: string[];
	    deviceFreeBytes: number;
	    accelerator: string;
	    readySeconds: number;
	    busy: boolean;
	    phase?: string;
	    done: number;
	    total: number;
	    error?: string;
	    distribution: string;
	    distributions: string[];
	    variants: string[];
	    flashAttentionModes: string[];
	    cacheTypes: string[];
	    offloadModes: string[];
	    expertOffloadModes: string[];
	    update: RuntimeUpdate;
	    restartPending: boolean;
	    mlxSupported: boolean;
	    mlxInstalled: boolean;
	    mlxVersion?: string;
	    mlxPython?: string;
	    mlxServer?: string;
	    mlxBusy: boolean;
	    mlxPhase?: string;
	    mlxError?: string;
	    models: ManagedStatus[];
	
	    static createFrom(source: any = {}) {
	        return new RuntimeStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.installed = source["installed"];
	        this.tag = source["tag"];
	        this.binary = source["binary"];
	        this.version = source["version"];
	        this.customBinary = source["customBinary"];
	        this.devices = source["devices"];
	        this.devicesError = source["devicesError"];
	        this.devicesMissingLibs = source["devicesMissingLibs"];
	        this.deviceFreeBytes = source["deviceFreeBytes"];
	        this.accelerator = source["accelerator"];
	        this.readySeconds = source["readySeconds"];
	        this.busy = source["busy"];
	        this.phase = source["phase"];
	        this.done = source["done"];
	        this.total = source["total"];
	        this.error = source["error"];
	        this.distribution = source["distribution"];
	        this.distributions = source["distributions"];
	        this.variants = source["variants"];
	        this.flashAttentionModes = source["flashAttentionModes"];
	        this.cacheTypes = source["cacheTypes"];
	        this.offloadModes = source["offloadModes"];
	        this.expertOffloadModes = source["expertOffloadModes"];
	        this.update = this.convertValues(source["update"], RuntimeUpdate);
	        this.restartPending = source["restartPending"];
	        this.mlxSupported = source["mlxSupported"];
	        this.mlxInstalled = source["mlxInstalled"];
	        this.mlxVersion = source["mlxVersion"];
	        this.mlxPython = source["mlxPython"];
	        this.mlxServer = source["mlxServer"];
	        this.mlxBusy = source["mlxBusy"];
	        this.mlxPhase = source["mlxPhase"];
	        this.mlxError = source["mlxError"];
	        this.models = this.convertValues(source["models"], ManagedStatus);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class SearchStatus {
	    providers: Record<string, boolean>;
	    order: string[];
	    braveQueriesThisMonth: number;
	    braveQuotaEstimate: number;
	    cacheMinutes: number;
	    secretBackend: string;
	
	    static createFrom(source: any = {}) {
	        return new SearchStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.providers = source["providers"];
	        this.order = source["order"];
	        this.braveQueriesThisMonth = source["braveQueriesThisMonth"];
	        this.braveQuotaEstimate = source["braveQuotaEstimate"];
	        this.cacheMinutes = source["cacheMinutes"];
	        this.secretBackend = source["secretBackend"];
	    }
	}
	export class SecretStatus {
	    backend: string;
	    keyring: boolean;
	    braveKeySet: boolean;
	    backendKeys?: Record<string, boolean>;
	    imageKeySet: boolean;
	    warning?: string;
	
	    static createFrom(source: any = {}) {
	        return new SecretStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.backend = source["backend"];
	        this.keyring = source["keyring"];
	        this.braveKeySet = source["braveKeySet"];
	        this.backendKeys = source["backendKeys"];
	        this.imageKeySet = source["imageKeySet"];
	        this.warning = source["warning"];
	    }
	}
	export class SendMessageRequest {
	    chatId: string;
	    text: string;
	    attachmentIds: string[];
	    backend: string;
	    model: string;
	    numCtx?: number;
	    regenerate: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SendMessageRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.chatId = source["chatId"];
	        this.text = source["text"];
	        this.attachmentIds = source["attachmentIds"];
	        this.backend = source["backend"];
	        this.model = source["model"];
	        this.numCtx = source["numCtx"];
	        this.regenerate = source["regenerate"];
	    }
	}
	export class StopDrawResult {
	    wasDrawing: boolean;
	    interrupted: boolean;
	    interruptError?: string;
	
	    static createFrom(source: any = {}) {
	        return new StopDrawResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.wasDrawing = source["wasDrawing"];
	        this.interrupted = source["interrupted"];
	        this.interruptError = source["interruptError"];
	    }
	}
	export class TerminalStatus {
	    program: string;
	    path: string;
	
	    static createFrom(source: any = {}) {
	        return new TerminalStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.program = source["program"];
	        this.path = source["path"];
	    }
	}
	export class UpscalerInstall {
	    busy: boolean;
	    phase: string;
	    done: number;
	    total: number;
	    error: string;
	    unsupported: string;
	    size: number;
	    version: string;
	
	    static createFrom(source: any = {}) {
	        return new UpscalerInstall(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.busy = source["busy"];
	        this.phase = source["phase"];
	        this.done = source["done"];
	        this.total = source["total"];
	        this.error = source["error"];
	        this.unsupported = source["unsupported"];
	        this.size = source["size"];
	        this.version = source["version"];
	    }
	}

}

export namespace research {
	
	export class ScrapeResult {
	    url: string;
	    finalUrl: string;
	    title: string;
	    markdown: string;
	    tokens: number;
	    contentType: string;
	    truncated: boolean;
	    cached: boolean;
	    pages?: number;
	    warning?: string;
	
	    static createFrom(source: any = {}) {
	        return new ScrapeResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.finalUrl = source["finalUrl"];
	        this.title = source["title"];
	        this.markdown = source["markdown"];
	        this.tokens = source["tokens"];
	        this.contentType = source["contentType"];
	        this.truncated = source["truncated"];
	        this.cached = source["cached"];
	        this.pages = source["pages"];
	        this.warning = source["warning"];
	    }
	}
	export class SearchResult {
	    title: string;
	    url: string;
	    snippet: string;
	    age?: string;
	    extra?: string[];
	    provider: string;
	    rank: number;
	
	    static createFrom(source: any = {}) {
	        return new SearchResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.title = source["title"];
	        this.url = source["url"];
	        this.snippet = source["snippet"];
	        this.age = source["age"];
	        this.extra = source["extra"];
	        this.provider = source["provider"];
	        this.rank = source["rank"];
	    }
	}
	export class SearchOutcome {
	    results: SearchResult[];
	    provider: string;
	    cached: boolean;
	    warning?: string;
	
	    static createFrom(source: any = {}) {
	        return new SearchOutcome(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.results = this.convertValues(source["results"], SearchResult);
	        this.provider = source["provider"];
	        this.cached = source["cached"];
	        this.warning = source["warning"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace runtime {
	
	export class MLXSnapshot {
	    path: string;
	    name: string;
	    complete: boolean;
	
	    static createFrom(source: any = {}) {
	        return new MLXSnapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.name = source["name"];
	        this.complete = source["complete"];
	    }
	}
	export class MLXScan {
	    dir: string;
	    snapshots: MLXSnapshot[];
	    ggufs: number;
	
	    static createFrom(source: any = {}) {
	        return new MLXScan(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.dir = source["dir"];
	        this.snapshots = this.convertValues(source["snapshots"], MLXSnapshot);
	        this.ggufs = source["ggufs"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class ModelFile {
	    path: string;
	    name: string;
	    model: string;
	    size: number;
	
	    static createFrom(source: any = {}) {
	        return new ModelFile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.name = source["name"];
	        this.model = source["model"];
	        this.size = source["size"];
	    }
	}
	export class ModelFolder {
	    path: string;
	    kind: string;
	    exists: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ModelFolder(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.kind = source["kind"];
	        this.exists = source["exists"];
	    }
	}
	export class ModelFolderScan {
	    dir: string;
	    files: ModelFile[];
	    truncated: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ModelFolderScan(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.dir = source["dir"];
	        this.files = this.convertValues(source["files"], ModelFile);
	        this.truncated = source["truncated"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace store {
	
	export class Derivation {
	    sourceAttachmentId: string;
	    sourceWidth?: number;
	    sourceHeight?: number;
	    method: string;
	    model?: string;
	    scale?: number;
	    sharpen?: number;
	    durationMs?: number;
	
	    static createFrom(source: any = {}) {
	        return new Derivation(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sourceAttachmentId = source["sourceAttachmentId"];
	        this.sourceWidth = source["sourceWidth"];
	        this.sourceHeight = source["sourceHeight"];
	        this.method = source["method"];
	        this.model = source["model"];
	        this.scale = source["scale"];
	        this.sharpen = source["sharpen"];
	        this.durationMs = source["durationMs"];
	    }
	}
	export class GenerationRecipe {
	    prompt: string;
	    sourcePrompt?: string;
	    captionModel?: string;
	    negativePrompt?: string;
	    width?: number;
	    height?: number;
	    steps?: number;
	    sampler?: string;
	    cfgScale?: number;
	    seed?: number;
	    model?: string;
	    hires?: boolean;
	    hiresScale?: number;
	    hiresDenoise?: number;
	    durationMs?: number;
	
	    static createFrom(source: any = {}) {
	        return new GenerationRecipe(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.prompt = source["prompt"];
	        this.sourcePrompt = source["sourcePrompt"];
	        this.captionModel = source["captionModel"];
	        this.negativePrompt = source["negativePrompt"];
	        this.width = source["width"];
	        this.height = source["height"];
	        this.steps = source["steps"];
	        this.sampler = source["sampler"];
	        this.cfgScale = source["cfgScale"];
	        this.seed = source["seed"];
	        this.model = source["model"];
	        this.hires = source["hires"];
	        this.hiresScale = source["hiresScale"];
	        this.hiresDenoise = source["hiresDenoise"];
	        this.durationMs = source["durationMs"];
	    }
	}
	export class Attachment {
	    id: string;
	    chatId: string;
	    messageId: string;
	    kind: string;
	    filename: string;
	    mime: string;
	    sizeBytes: number;
	    sha256: string;
	    pages: number;
	    tokenEstimate: number;
	    placement: string;
	    documentId: string;
	    warning?: string;
	    // Go type: time
	    createdAt: any;
	    savedPath?: string;
	    projectPath?: string;
	    generation?: GenerationRecipe;
	    derivation?: Derivation;
	
	    static createFrom(source: any = {}) {
	        return new Attachment(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.chatId = source["chatId"];
	        this.messageId = source["messageId"];
	        this.kind = source["kind"];
	        this.filename = source["filename"];
	        this.mime = source["mime"];
	        this.sizeBytes = source["sizeBytes"];
	        this.sha256 = source["sha256"];
	        this.pages = source["pages"];
	        this.tokenEstimate = source["tokenEstimate"];
	        this.placement = source["placement"];
	        this.documentId = source["documentId"];
	        this.warning = source["warning"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.savedPath = source["savedPath"];
	        this.projectPath = source["projectPath"];
	        this.generation = this.convertValues(source["generation"], GenerationRecipe);
	        this.derivation = this.convertValues(source["derivation"], Derivation);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Chat {
	    id: string;
	    title: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	    backend: string;
	    model: string;
	    systemPrompt: string;
	    systemPromptPreset: string;
	    mode: string;
	    workspace: string;
	    temperature: number;
	    topP: number;
	    numCtx: number;
	    archived: boolean;
	    autoCompact: boolean;
	    compactThreshold: number;
	    keepRecentTurns: number;
	    searchFirst: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Chat(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.backend = source["backend"];
	        this.model = source["model"];
	        this.systemPrompt = source["systemPrompt"];
	        this.systemPromptPreset = source["systemPromptPreset"];
	        this.mode = source["mode"];
	        this.workspace = source["workspace"];
	        this.temperature = source["temperature"];
	        this.topP = source["topP"];
	        this.numCtx = source["numCtx"];
	        this.archived = source["archived"];
	        this.autoCompact = source["autoCompact"];
	        this.compactThreshold = source["compactThreshold"];
	        this.keepRecentTurns = source["keepRecentTurns"];
	        this.searchFirst = source["searchFirst"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Chunk {
	    id: number;
	    documentId: string;
	    chatId: string;
	    ordinal: number;
	    heading: string;
	    page: number;
	    text: string;
	    tokens: number;
	
	    static createFrom(source: any = {}) {
	        return new Chunk(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.documentId = source["documentId"];
	        this.chatId = source["chatId"];
	        this.ordinal = source["ordinal"];
	        this.heading = source["heading"];
	        this.page = source["page"];
	        this.text = source["text"];
	        this.tokens = source["tokens"];
	    }
	}
	export class Compaction {
	    id: string;
	    chatId: string;
	    trigger: string;
	    fromSeq: number;
	    toSeq: number;
	    summary: string;
	    focus: string;
	    tokensBefore: number;
	    tokensAfter: number;
	    model: string;
	    backend: string;
	    // Go type: time
	    createdAt: any;
	
	    static createFrom(source: any = {}) {
	        return new Compaction(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.chatId = source["chatId"];
	        this.trigger = source["trigger"];
	        this.fromSeq = source["fromSeq"];
	        this.toSeq = source["toSeq"];
	        this.summary = source["summary"];
	        this.focus = source["focus"];
	        this.tokensBefore = source["tokensBefore"];
	        this.tokensAfter = source["tokensAfter"];
	        this.model = source["model"];
	        this.backend = source["backend"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class Document {
	    id: string;
	    attachmentId: string;
	    chatId: string;
	    title: string;
	    text: string;
	    tokenCount: number;
	    pageCount: number;
	    sha256: string;
	    sourceUrl: string;
	    // Go type: time
	    createdAt: any;
	
	    static createFrom(source: any = {}) {
	        return new Document(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.attachmentId = source["attachmentId"];
	        this.chatId = source["chatId"];
	        this.title = source["title"];
	        this.text = source["text"];
	        this.tokenCount = source["tokenCount"];
	        this.pageCount = source["pageCount"];
	        this.sha256 = source["sha256"];
	        this.sourceUrl = source["sourceUrl"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class Source {
	    index: number;
	    title: string;
	    url: string;
	
	    static createFrom(source: any = {}) {
	        return new Source(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.index = source["index"];
	        this.title = source["title"];
	        this.url = source["url"];
	    }
	}
	export class ToolCallRecord {
	    id: string;
	    messageId: string;
	    chatId: string;
	    name: string;
	    argsJson: string;
	    resultJson: string;
	    status: string;
	    durationMs: number;
	    error: string;
	    sourceTokens: number;
	    contextTokens: number;
	    condensed: boolean;
	    withheld: boolean;
	    // Go type: time
	    createdAt: any;
	
	    static createFrom(source: any = {}) {
	        return new ToolCallRecord(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.messageId = source["messageId"];
	        this.chatId = source["chatId"];
	        this.name = source["name"];
	        this.argsJson = source["argsJson"];
	        this.resultJson = source["resultJson"];
	        this.status = source["status"];
	        this.durationMs = source["durationMs"];
	        this.error = source["error"];
	        this.sourceTokens = source["sourceTokens"];
	        this.contextTokens = source["contextTokens"];
	        this.condensed = source["condensed"];
	        this.withheld = source["withheld"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Message {
	    id: string;
	    chatId: string;
	    seq: number;
	    role: string;
	    content: string;
	    reasoning: string;
	    name: string;
	    toolCallsJson: string;
	    toolCallId: string;
	    status: string;
	    // Go type: time
	    createdAt: any;
	    backend: string;
	    model: string;
	    promptTokens: number;
	    cacheHitTokens?: number;
	    completionTokens: number;
	    tokensPerSecond: number;
	    ttftMs: number;
	    generationMs: number;
	    stopReason: string;
	    statsSource: string;
	    attachments?: Attachment[];
	    toolCalls?: ToolCallRecord[];
	    sources?: Source[];
	
	    static createFrom(source: any = {}) {
	        return new Message(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.chatId = source["chatId"];
	        this.seq = source["seq"];
	        this.role = source["role"];
	        this.content = source["content"];
	        this.reasoning = source["reasoning"];
	        this.name = source["name"];
	        this.toolCallsJson = source["toolCallsJson"];
	        this.toolCallId = source["toolCallId"];
	        this.status = source["status"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.backend = source["backend"];
	        this.model = source["model"];
	        this.promptTokens = source["promptTokens"];
	        this.cacheHitTokens = source["cacheHitTokens"];
	        this.completionTokens = source["completionTokens"];
	        this.tokensPerSecond = source["tokensPerSecond"];
	        this.ttftMs = source["ttftMs"];
	        this.generationMs = source["generationMs"];
	        this.stopReason = source["stopReason"];
	        this.statsSource = source["statsSource"];
	        this.attachments = this.convertValues(source["attachments"], Attachment);
	        this.toolCalls = this.convertValues(source["toolCalls"], ToolCallRecord);
	        this.sources = this.convertValues(source["sources"], Source);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	

}

export namespace tools {
	
	export class UpscalerStatus {
	    enabled: boolean;
	    available: boolean;
	    path: string;
	    models: string[];
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new UpscalerStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.available = source["available"];
	        this.path = source["path"];
	        this.models = source["models"];
	        this.error = source["error"];
	    }
	}

}

export namespace workspace {
	
	export class DiffLine {
	    kind: string;
	    text: string;
	    old: number;
	    new: number;
	
	    static createFrom(source: any = {}) {
	        return new DiffLine(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.text = source["text"];
	        this.old = source["old"];
	        this.new = source["new"];
	    }
	}
	export class Hunk {
	    lines: DiffLine[];
	
	    static createFrom(source: any = {}) {
	        return new Hunk(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.lines = this.convertValues(source["lines"], DiffLine);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

