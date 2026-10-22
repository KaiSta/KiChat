import { useEffect, useRef, useState } from 'react'
import { App, formatBytes, formatNumber, type config, type main, type tools } from '../../lib/api'
import { errText, useStore } from '../../store'
import { Section } from './shared'

const MODEL_PATTERNS = ['*.gguf', '*.safetensors', '*']

/**
 * A base size the draw buttons can fill in, and — for the two 16:9 entries — the second
 * pass that lands on the screen size its label names.
 *
 * `labelKey` null renders the base as a square (`512²`). `hintKey` is the tooltip; an
 * empty one uses the generic square-size sentence. `scale` 0 leaves the stored
 * enlargement alone, which is what a square does: it names a base, not an output.
 */
type SizePreset = {
  labelKey: string | null
  hintKey: string
  width: number
  height: number
  scale: number
}

/**
 * The sizes diffusion work actually starts from: the three trained-at squares, plus the
 * two 16:9 bases whose second pass lands exactly on a screen size.
 *
 * Three squares rather than a picker of everything: these are the widths the models in
 * the wild were trained at or near, and the point of them is to spare a user typing the
 * same number into two boxes. Anything else is still typed, so this adds shortcuts and
 * takes nothing away.
 *
 * The 16:9 pair is arithmetic, not taste. 1280×720 × 1.5 = 1920×1080 (FHD) and
 * 1920×1080 × 2 = 3840×2160 (4K); both bases are multiples of the 8-pixel latent grid
 * and inside MaxDimension (2048 a side), and both outputs are inside
 * MaxOutputDimension (4096), so `images.Request.OutputSize` predicts the same numbers
 * the backend is asked for. 4K cannot be reached at the default 1.5× — 3840/1.5 is
 * 2560, past the base bound — which is why that entry carries its own 2.
 *
 * The switch stays the user's: the preset stores the enlargement but does not turn the
 * second pass on, so someone whose card cannot afford it still gets the base picture,
 * and the tooltip says what the pass would make of it.
 */
const SIZE_PRESETS: SizePreset[] = [
  { labelKey: null, hintKey: '', width: 512, height: 512, scale: 0 },
  { labelKey: null, hintKey: '', width: 768, height: 768, scale: 0 },
  { labelKey: null, hintKey: '', width: 1024, height: 1024, scale: 0 },
  {
    labelKey: 'settings.images.sizePresetFhd',
    hintKey: 'settings.images.sizePresetFhdHint',
    width: 1280,
    height: 720,
    scale: 1.5,
  },
  {
    labelKey: 'settings.images.sizePresetUhd',
    hintKey: 'settings.images.sizePresetUhdHint',
    width: 1920,
    height: 1080,
    scale: 2,
  },
]

// The second-pass bounds, mirrored from internal/images/limits.go. They are not bridge
// constants — Go's exported names do not reach JavaScript — so the panel states its own
// and the backend clamps again when the setting is saved.
// The hosted model ids, spelled as the path segment NVIDIA's route takes
// (`/v1/genai/<publisher>/<model>`). They are a constant rather than the status list because
// the panel has to offer a model before any key exists: choosing one and adding the key are
// the two things a user does here, in that order. The list is short on purpose — an id the
// vendor does not serve answers 404, so guessing more names would only add broken choices.
//
// Order is the shipped default first: schnell is the Apache-2.0 one. Klein is second because
// it is the hosted model measured to answer on a free-trial key.
const NIM_MODELS = [
  'black-forest-labs/flux.1-schnell',
  'black-forest-labs/flux-3-image-klein',
  'black-forest-labs/flux.1-dev',
]

/**
 * The stored hosted id, then the offered ones.
 *
 * A `<select>` whose `value` matches no `<option>` shows the *first* option while leaving the
 * state untouched. That is how a draw went out for `flux-3-image-klein` while the panel
 * displayed `flux.1-schnell`: the stored id came from an earlier build of this field, no option
 * matched it, and the only thing the user saw was NVIDIA's 404 naming a model they had never
 * picked. Putting the stored value first makes the control say what will be sent.
 */
function nimModelOptions(stored: string | undefined): string[] {
  const value = (stored ?? '').trim()
  if (!value || NIM_MODELS.includes(value)) return NIM_MODELS
  return [value, ...NIM_MODELS]
}

/** Whether a hosted id is one this build does not list, and therefore cannot vouch for. */
function nimModelUnlisted(id: string | undefined): boolean {
  const value = (id ?? '').trim()
  return value !== '' && !NIM_MODELS.includes(value)
}

const IMAGE_HIRES_LIMITS = {
  minScale: 1.25,
  maxScale: 2,
  defaultScale: 1.5,
  minDenoise: 0.2,
  maxDenoise: 0.6,
  defaultDenoise: 0.4,
}

/**
 * Fills one CLI path from the native file dialog.
 *
 * A cancelled dialog answers with an empty string, and that must leave the field alone: a
 * user who opened the picker to look around would otherwise lose the path that was already
 * there.
 */
async function pickFile(
  update: (changes: Partial<config.ImageSettings>) => void,
  title: string,
  patterns: string[],
  field: keyof config.ImageSettings,
) {
  try {
    const path = await App.PickImageFile(title, patterns)
    if (path) update({ [field]: path } as Partial<config.ImageSettings>)
  } catch (err) {
    useStore.getState().toast('error', errText(err))
  }
}

/**
 * Fills the program folder from the native folder dialog.
 *
 * A folder, not a file: that is the shape a stable-diffusion.cpp download arrives in,
 * and KiChat finds the program inside it. The binding checks the choice before
 * answering, so a folder with no `sd-cli` in it comes back as an error — which is why
 * this reports one instead of quietly storing a folder nothing will be drawn with.
 *
 * A cancelled dialog answers with an empty string and must leave the field alone, the
 * same rule the file picker follows.
 */
async function pickImageFolder(
  update: (changes: Partial<config.ImageSettings>) => void,
  title: string,
) {
  try {
    const dir = await App.PickImageFolder(title)
    if (dir) update({ cliFolder: dir })
  } catch (err) {
    useStore.getState().toast('error', errText(err))
  }
}

/**
 * One path in the CLI backend's command line: a labelled field with a browse button.
 *
 * The eight of them are the same control with different text, and writing them out eight
 * times would mean eight chances to forget the button or the hint.
 *
 * `disabled` and `note` exist for the one field whose value can be beside the point: the
 * explicit program path, which the backend ignores outright while a program folder is set.
 * It is drawn dimmed rather than hidden, because a stored path that vanishes from the panel
 * is a path its owner cannot clear — and the sentence that has to be read without hovering
 * anything is the `note`, since `hint` is a tooltip.
 */
function PathField({
  label,
  hint,
  value,
  placeholder,
  onChange,
  onPick,
  disabled,
  note,
}: {
  label: string
  hint: string
  value: string
  placeholder?: string
  onChange: (value: string) => void
  onPick: () => void
  disabled?: boolean
  note?: string
}) {
  const t = useStore((s) => s.t)
  return (
    <label className="text-[12px]" title={hint}>
      {label}
      <div className="mt-1 flex items-center gap-2">
        <input
          className="input mono flex-1"
          value={value}
          placeholder={placeholder}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        />
        <button type="button" className="btn btn-sm" disabled={disabled} onClick={onPick}>
          {t('settings.images.browse')}
        </button>
      </div>
      {note ? <span className="faint mt-1 block text-[11px]">{note}</span> : null}
    </label>
  )
}

/**
 * The picture backend, on a tab of its own.
 *
 * It began as a section of the backends tab and moved out once the setup outgrew a
 * row beside a chat backend. Three things have no counterpart there: the server has
 * to be started with its API exposed and a checkpoint installed, the generation
 * fields each need a sentence about what leaving them empty means, and the
 * connection test spends real GPU seconds drawing a picture. None of that applies to
 * a chat backend — there is no context window to show, no per-turn model to pick,
 * and a request answers with a picture instead of a stream. The one hard rule,
 * stated under the fields, is that an empty box leaves the setting to the server: a
 * WebUI user has already chosen a sampler, a step count and a size, and filling
 * those in with guesses would replace their setup.
 */
export function ImagesTab({
  draft,
  patch,
  onApply,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
  onApply: (next?: config.Settings) => Promise<void>
}) {
  const t = useStore((s) => s.t)
  // Keys built from a value the backend reports, rather than written out here.
  const tDynamic = useStore((s) => s.tDynamic)
  const [status, setStatus] = useState<main.ImageStatus | null>(null)
  const [checking, setChecking] = useState(false)
  const [testing, setTesting] = useState(false)
  // The neural upscaler's setup. It is read from the *saved* settings, like every other
  // status on this screen, so the Test button commits the draft first and the reading
  // describes what was tested rather than what is still being typed.
  const [upscaler, setUpscaler] = useState<tools.UpscalerStatus | null>(null)
  const [upscalerBusy, setUpscalerBusy] = useState(false)
  // The download this panel can do itself: what the backend would fetch for this machine,
  // and its progress while it does. It is read from the backend rather than hard-coded
  // because the archive and its size differ per platform — 46.9 MB on Linux, 51.8 MB on
  // macOS — and a row that named one of them would be wrong on the others. `installing`
  // covers the moment between the press and the first reading.
  const [upscalerInstall, setUpscalerInstall] = useState<main.UpscalerInstall | null>(null)
  const [installing, setInstalling] = useState(false)
  // Set by the Stop button beside the test, and read by the rejection it causes: a stopped
  // test rejects like a failed one, and only this flag can tell the two apart.
  const stoppingTest = useRef(false)
  const images = draft.images ?? ({} as config.ImageSettings)
  // The kind key is stored empty for the server backend, so the selector's two values are
  // this string and the empty one. It is read once here rather than compared at every field:
  // which half of the form is on screen has to be one answer.
  const isCLI = images.kind === 'cli'
  // A program folder is a statement about a *place*, and the backend lets it win outright
  // (images.ResolveProgram): the typed program path is the fallback for an install that has
  // no folder at all. Two fields that both look required are exactly what reads as "seems
  // double", so the panel says which of the two is supplying the executable.
  const folderSet = (images.cliFolder ?? '').trim() !== ''
  // The program the folder resolved to, and only while it still describes the folder in the
  // draft: `status` answers for the *saved* settings, so a path printed under a folder the
  // user has just finished typing would be a reading of some other folder.
  const foundProgram =
    folderSet && status?.folder === images.cliFolder ? status.binary ?? '' : ''
  // The resolved profile is a reading of the *saved* settings, like the found program: it
  // comes from the diffusion model's own header — its tensor layout — or, when that cannot
  // be read, from its file name. Printing it under a path the user is still typing would
  // describe a different model, so it is shown only while the setting is automatic and the
  // saved file list really holds the draft's path.
  const resolvedPipeline =
    !images.pipeline &&
    status?.files?.includes((images.cliDiffusionModel ?? '').trim()) &&
    status.pipeline &&
    status.pipeline !== 'generic'
      ? { name: status.pipelineName ?? '', source: status.pipelineSource ?? '' }
      : null
  // The third kind: the *same* local build, run as a server KiChat starts and stops. It is
  // not a variant of the one-shot kind in the UI either — the fields are shared because the
  // flags are, but the process, the port and the lifetime are the server's own.
  const isSD = images.kind === 'sdserver'
  // The hosted kind: nothing on this machine, no folder, no checkpoint — one HTTPS call with
  // the account's NVIDIA key. It is neither local nor a server of the user's, which is why it
  // gets its own branch rather than a fourth option behind the shared fields.
  const isNIM = images.kind === 'nim'
  // Both local kinds are one build from one archive in two lifetimes, which is what makes the
  // folder and the whole model set *shared* rather than duplicated per kind: the same fields
  // feed `sd-cli -p …` and `sd-server --listen-port …`.
  const isLocal = isCLI || isSD
  // Which executable the folder is expected to hold. The backend reports it rather than the
  // view hard-coding one of the two names, so a renamed row cannot describe the other
  // program.
  const program = status?.program || (isSD ? 'sd-server' : 'sd-cli')
  const server = status?.server
  const sdState = server?.state || 'stopped'
  const sdRunning = isSD && sdState === 'running'
  const sdFailed = isSD && sdState === 'failed'
  // What an empty field means, for the one field where the backend decides: the checkpoint.
  // An empty sampler, size, step count or negative prompt does *not* mean this — the model
  // decides those, and the shared fields below say so themselves.
  const auto = t('settings.images.auto')

  // The builds stable-diffusion.cpp publishes for this machine, and which one the panel
  // is pointing the user at. KiChat does not download the program, so these rows are the
  // whole of how a user is told which archive to take: the project's own list, filtered
  // to this platform by the backend — a CUDA archive is published for Windows and not
  // for Linux, and offering one would send them to a file that does not exist.
  const builds = status?.builds ?? []
  const [sdBuild, setSdBuild] = useState('')
  const [serverBusy, setServerBusy] = useState(false)
  // What is already installed is the useful default — "you are on the Vulkan build, here
  // is the file it came from" — and the first published build otherwise, which is the
  // CPU one and runs anywhere.
  const chosenBuild =
    builds.find((b) => b.id === sdBuild) ??
    builds.find((b) => b.id === (status?.build || 'cpu')) ??
    builds[0]

  const update = (changes: Partial<config.ImageSettings>) =>
    patch({ images: { ...images, ...changes } as config.ImageSettings })

  useEffect(() => {
    let cancelled = false
    App.GetUpscalerStatus()
      .then((value) => {
        if (!cancelled) setUpscaler(value)
      })
      .catch(() => {
        if (!cancelled) setUpscaler(null)
      })
    App.GetUpscalerInstall()
      .then((value) => {
        if (!cancelled) setUpscalerInstall(value)
      })
      .catch(() => {
        if (!cancelled) setUpscalerInstall(null)
      })
    return () => {
      cancelled = true
    }
  }, [])

  /* The setup test is a real 16×16 upscale, which is the only check that covers the
   * Vulkan driver as well as the executable. The draft is committed first for the same
   * reason the backend test is: the saved settings are what a run reads, and testing
   * yesterday's path would report a failure about a mistake already corrected. */
  const testUpscaler = async () => {
    setUpscalerBusy(true)
    try {
      await onApply({ ...draft, images })
      const path = await App.TestUpscaler()
      setUpscaler(await App.GetUpscalerStatus())
      useStore.getState().toast('info', `${t('settings.images.upscalerTestOk')} ${path}`)
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    } finally {
      setUpscalerBusy(false)
    }
  }

  const pickUpscaler = async () => {
    try {
      const path = await App.PickUpscalerBinary(t('settings.images.upscalerPickTitle'))
      if (path) update({ esrganBinary: path })
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    }
  }

  /*
   * The download the app can do itself.
   *
   * It waits here rather than in a mount effect, because the thing being waited for is one
   * press of one button: the loop ends when the backend says the download is over, and a
   * user who leaves this tab mid-download loses only the progress row — the install
   * finishes on the backend either way, and coming back reads the same state.
   *
   * The switch and the path are taken from the backend's own status afterwards rather than
   * from the press: the install writes them into the *saved* settings, so the draft would
   * otherwise disagree with what the app is really configured with.
   */
  const downloadUpscaler = async () => {
    setInstalling(true)
    try {
      await App.InstallUpscaler()
      for (;;) {
        await new Promise((done) => setTimeout(() => done(null), 500))
        const next = await App.GetUpscalerInstall()
        setUpscalerInstall(next)
        if (next.busy) continue
        if (next.error) {
          useStore.getState().toast('error', errText(next.error))
          return
        }
        const status = await App.GetUpscalerStatus()
        setUpscaler(status)
        update({ esrganBinary: status.path ?? '', upscalerEnabled: true })
        useStore.getState().toast('info', t('settings.images.upscalerInstalled'))
        return
      }
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    } finally {
      setInstalling(false)
    }
  }

  // Read again without the mount's "checking" dance: the server row polls this while a
  // model loads, and a spinner that reappears every 1.2 s would be noise rather than a
  // reading.
  const readStatus = async () => {
    try {
      setStatus(await App.GetImageStatus())
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    }
  }

  /*
   * The managed server's two buttons. Both commit the draft first, for the reason the
   * connection test does: the settings are what a start reads, so starting an unsaved
   * configuration would load the previous model and report a failure about something the
   * user already corrected.
   *
   * Start doubles as Restart — the backend has one call for both, because sd-server loads
   * one pipeline at startup and has no way to be told about another. The label says which of
   * the two the click will be, and both are the user's own click: nothing in this app
   * reloads a model that may have taken minutes to load.
   */
  const startServer = async () => {
    setServerBusy(true)
    try {
      await onApply({ ...draft, images })
      await App.StartImageServer()
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    } finally {
      setServerBusy(false)
      await readStatus()
    }
  }

  const stopServer = async () => {
    setServerBusy(true)
    try {
      await App.StopImageServer()
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    } finally {
      setServerBusy(false)
      await readStatus()
    }
  }

  // Only while something is moving. A server loading a model takes minutes and the row has to
  // follow it, while an idle window must not ask for a status twice a second for the rest of
  // the session.
  useEffect(() => {
    if (sdState !== 'starting') return
    const id = window.setInterval(() => void readStatus(), 1200)
    return () => window.clearInterval(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sdState])

  // A save can leave the running server loading a model the settings no longer describe, and
  // "restart needed" is a *reading* the backend answers rather than a field the panel keeps —
  // so the row has to ask again after one. Without this, the badge for the very edit that
  // creates the condition would only appear once the section was left and re-entered.
  //
  // Keyed on the stored command line rather than on the draft (a keystroke is not a save), and
  // only while a server is up: a stopped one reads these settings when it starts, so there is
  // nothing to ask about and no reason to probe a backend the user may not be running.
  const storedImages = useStore((s) => s.settings?.images)
  const storedStartup = storedImages ? startupKey(storedImages) : ''
  useEffect(() => {
    if (sdState !== 'running') return
    void readStatus()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [storedStartup])

  /*
   * Profiles: the saved configurations, one per checkpoint or per pipeline a user
   * switches between. They are part of the settings blob, so every one of the three
   * buttons below commits through onApply instead of only writing into the draft — a
   * profile that disappeared on the way to another tab is the opposite of what a
   * "save" button promises, and the point of the list is to make switching a single
   * click.
   */
  const profiles = draft.imageProfiles ?? []
  const [picked, setPicked] = useState('')
  const [profileName, setProfileName] = useState('')
  // Deleting a configuration is not undoable, so it asks twice, the way the picture
  // store's clear button does.
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [profileBusy, setProfileBusy] = useState(false)
  const trimmedName = profileName.trim()
  // The button says which of the two it will do, because "save" and "update" are the
  // same click on the same form and only the name decides which one the user gets.
  const replacing = profiles.some((p) => p.name === trimmedName)
  const selected = profiles.find((p) => p.name === picked)

  /*
   * What a profile holds, and what it deliberately leaves behind: the drawing
   * configuration, not the feature switch (applying one must not be able to turn image
   * generation off), not the export folder (that is where the user keeps pictures, not
   * how they are made) and not the credential. The server's "user:pass" belongs to the
   * server a profile is a way of using, so it stays with the feature as one keyring
   * entry — which is also what keeps a secret out of the settings blob, where a copy of
   * it would sit in plain text.
   */
  const profileSettings = (): config.ImageSettings =>
    ({ ...images, enabled: false, saveFolder: '', apiKey: '' }) as config.ImageSettings

  const upsertProfile = (name: string): config.ImageProfile[] => {
    const at = profiles.findIndex((p) => p.name === name)
    if (at < 0) return [...profiles, { name, settings: profileSettings() }]
    const next = profiles.slice()
    // Written back in place: a row that jumped to the end of the list on every update
    // would be a dropdown reordering itself under the cursor.
    next[at] = { ...next[at], settings: profileSettings() }
    return next
  }

  const saveProfile = async () => {
    if (!trimmedName) return
    const next = upsertProfile(trimmedName)
    setProfileBusy(true)
    try {
      patch({ imageProfiles: next })
      // Handed in rather than read from state: setState has not landed yet, which is
      // the same reason the connection test passes its payload along.
      await onApply({ ...draft, images, imageProfiles: next })
      setPicked(trimmedName)
    } finally {
      setProfileBusy(false)
    }
  }

  /*
   * Whether applying would have to reload the model: the managed server loads one pipeline when
   * it starts and cannot be told about another, so a profile that names a different model set is
   * a different *process*. The comparison is over the fields a start reads (startupKey) and the
   * label of the button says so before the click, because a reload is minutes — and the click
   * performs it, because picking a model is how a user asks to draw with it.
   */
  const applyRestarts =
    sdRunning &&
    !!selected &&
    // The profile has to *be* the managed server as well. One that switches the kind away
    // leaves the running process alone — it keeps the model it loaded, and the row above is
    // still how it is stopped — so asking for a start here would only come back with "the
    // picture generation is not set to the local server", about a choice the user just made.
    selected.settings.kind === 'sdserver' &&
    startupKey(selected.settings) !== startupKey(images)

  const applyProfile = async () => {
    if (!selected) return
    setProfileBusy(true)
    try {
      // The three fields the profile does not describe come from the draft, so
      // switching configurations never switches the feature off or moves the export
      // folder.
      const next = {
        ...selected.settings,
        enabled: images.enabled,
        saveFolder: images.saveFolder,
        apiKey: images.apiKey,
      } as config.ImageSettings
      patch({ images: next })
      await onApply({ ...draft, images: next })
      if (applyRestarts) {
        // After the save: the settings are what a start reads, so restarting first would
        // load the model the user just moved away from.
        await App.StartImageServer()
        await readStatus()
      }
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    } finally {
      setProfileBusy(false)
    }
  }

  const deleteProfile = async () => {
    const name = picked
    const next = profiles.filter((p) => p.name !== name)
    setProfileBusy(true)
    try {
      patch({ imageProfiles: next })
      await onApply({ ...draft, images, imageProfiles: next })
      setPicked('')
      setProfileName('')
    } finally {
      setProfileBusy(false)
      setConfirmDelete(false)
    }
  }

  // Read once on mount. The probe is a request to a server the user may not be
  // running, so re-asking on every keystroke in the draft would hammer a port that
  // is probably closed; the test button is what re-reads it deliberately. The
  // effect is written out rather than shared with that button because it depends
  // on nothing: a helper would either be a dependency that changes every render or
  // a stale closure, and an empty dependency list is neither.
  useEffect(() => {
    let cancelled = false
    setChecking(true)
    App.GetImageStatus()
      .then((next) => {
        if (!cancelled) setStatus(next)
      })
      .catch((err) => {
        if (!cancelled) useStore.getState().toast('error', errText(err))
      })
      .finally(() => {
        if (!cancelled) setChecking(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  const test = async () => {
    setTesting(true)
    stoppingTest.current = false
    try {
      // The app probes what was *saved*, so the draft has to be committed first:
      // testing an address that was just typed would otherwise test the previous
      // one and report a failure about a server the user already corrected.
      await onApply({ ...draft, images })
      // A listing cannot tell a server that is up from one that can draw: a WebUI
      // with no checkpoint loaded answers `/sdapi/v1/sd-models` and then fails
      // every generation. So the button draws one small picture and throws it
      // away, which is the only check that exercises the route the tool uses.
      const report = await App.TestImageBackend()
      setStatus(await App.GetImageStatus())
      useStore.getState().toast('info', report)
    } catch (err) {
      // A stop is not a failure: the click that ended it is the user's own, and
      // reporting it in red would describe a working server as a broken one. Same
      // split as the picture card's Stop.
      if (stoppingTest.current) {
        useStore.getState().toast('info', t('images.drawStopped'))
      } else {
        useStore.getState().toast('error', errText(err))
      }
      // The row is re-read even so, because what a failed generation needs to be
      // read against is whether the address answers at all: a WebUI that is
      // running with nothing loaded is a different repair from one that is not up.
      try {
        setStatus(await App.GetImageStatus())
      } catch (statusErr) {
        useStore.getState().toast('error', String(statusErr))
      }
    } finally {
      stoppingTest.current = false
      setTesting(false)
    }
  }

  /* Stops a test draw. The button says "test" and looks like a listing, but on the one-shot
   * kind it is a *model load* followed by a draw, and the bound behind it is that
   * backend's own — up to half an hour — so without this the panel would sit on a running
   * button with nothing to press. The id is the fixed one the backend registers the test
   * under (`image-test` in api.go), since a test has no attachment to name it by. Nothing is
   * stored either way: the picture is discarded when it arrives, so a stopped test leaves
   * the settings exactly as they were. */
  const stopTest = async () => {
    stoppingTest.current = true
    try {
      const result = await App.StopImageDraw('image-test')
      // Nothing was running — a second click, or a draw that finished between the render
      // and the click. Clearing the flag is what keeps the next rejection reported as the
      // failure it is, and nothing is toasted for a stop that did not happen.
      if (!result.wasDrawing) stoppingTest.current = false
    } catch (err) {
      stoppingTest.current = false
      useStore.getState().toast('error', errText(err))
    }
  }

  return (
    <Section title={t('settings.images')} hint={t('settings.imagesHint')}>
      <div className="mb-2 flex items-center gap-2">
        <input
          type="checkbox"
          checked={images.enabled ?? false}
          aria-label={t('settings.images.enabled')}
          title={t('settings.images.enabled')}
          onChange={(e) => update({ enabled: e.target.checked })}
        />
        <span className="flex-1 text-[13px]">{t('settings.images.enabled')}</span>
        {/* Four states, for the reasons the backend rows already learned. Switched
            off is not "unreachable" — nothing was sent. And a server with no
            checkpoint installed is reachable and still unable to draw, which is
            its own sentence rather than a failure of the address. */}
        {!images.enabled ? (
          <span className="badge">{t('settings.images.off')}</span>
        ) : status?.error ? (
          <span className="badge badge-danger" title={errText(status.error)}>
            {t('settings.fail')}
          </span>
        ) : isSD ? (
          /* The managed server is not probed as an address: it may simply be stopped, which
             is a state and not a failure, and the row below carries the same word plus the
             button that changes it. */
          <span
            className={`badge ${sdRunning && status?.ok ? 'badge-ok' : sdFailed ? 'badge-danger' : ''}`}
            title={server?.error ? errText(server.error) : undefined}
          >
            {sdRunning && status?.ok
              ? `${t('settings.images.ok')} · ${status.models.length} ${t('settings.images.models')}`
              : tDynamic(`settings.managed.state.${sdState}`)}
          </span>
        ) : status?.ok ? (
          // The CLI backend has no listing to count, so its badge counts the files it will
          // load instead. Reporting "0 models" on a configured backend would read as a
          // broken setup rather than as a question this backend cannot be asked.
          // The hosted kind counts models rather than checkpoints: nothing in its list is a
          // file on this disk, and the wrong noun here sends a user looking for one.
          isCLI ? (
            <span className="badge badge-ok">
              {t('settings.images.ok')} · {status.files.length} {t('settings.images.files')}
            </span>
          ) : (
            <span className={`badge ${status.models.length > 0 ? 'badge-ok' : 'badge-warn'}`}>
              {t('settings.images.ok')} · {status.models.length}{' '}
              {isNIM ? t('settings.images.nimModels') : t('settings.images.models')}
            </span>
          )
        ) : (
          <span className="badge">{checking ? '…' : t('settings.notChecked')}</span>
        )}
      </div>
      {!images.enabled ? (
        /* The sentence the rest of this panel depends on: with the feature off there is no
           tool for the model to call and every field below is inert, which a badge alone
           does not say to somebody who has just finished filling them in. */
        <p className="faint mb-2 text-[11px]">{t('settings.images.offHint')}</p>
      ) : null}

      {/* Profiles come before the fields rather than after them: picking one is how a user
          starts, and the row underneath is what the pick applies. Their own save commits
          at once — see saveProfile — so the list is never only in the draft.

          Hidden for the hosted kind, because a profile is a local model set: steps, a
          guidance scale and a pipeline, none of which the vendor's route accepts. Offering
          the row would let Apply change settings that nothing reads. */}
      <div
        className={isNIM ? 'hidden' : 'mb-3 border-t pt-3'}
        style={{ borderColor: 'var(--border)' }}
      >
        <div className="text-[12px]">{t('settings.images.profiles')}</div>
        <p className="faint mb-2 text-[11px]">{t('settings.images.profilesHint')}</p>
        <div className="flex flex-wrap items-center gap-2">
          <select
            className="input"
            aria-label={t('settings.images.profilePick')}
            title={t('settings.images.profilePick')}
            value={picked}
            onChange={(e) => {
              setPicked(e.target.value)
              // Picking also fills the name box, so the same pick is how a stored
              // configuration is corrected: pick it, change a field, press update.
              setProfileName(e.target.value)
              setConfirmDelete(false)
            }}
          >
            <option value="">{t('settings.images.profileNone')}</option>
            {profiles.map((p) => (
              <option key={p.name} value={p.name}>
                {p.name}
              </option>
            ))}
          </select>
          <button
            type="button"
            className="btn btn-sm"
            disabled={!selected || profileBusy}
            onClick={() => void applyProfile()}
          >
            {profileBusy
              ? t('settings.images.profileBusy')
              : applyRestarts
                ? t('settings.images.profileApplyRestart')
                : t('settings.images.profileApply')}
          </button>
          {confirmDelete ? (
            <>
              <span className="text-[11px]" style={{ color: 'var(--danger)' }}>
                {t('settings.images.profileDeleteConfirm', { name: picked })}
              </span>
              <button
                type="button"
                className="btn btn-sm btn-danger"
                disabled={profileBusy}
                onClick={() => void deleteProfile()}
              >
                {t('settings.images.profileDeleteYes')}
              </button>
              <button
                type="button"
                className="btn btn-sm"
                disabled={profileBusy}
                onClick={() => setConfirmDelete(false)}
              >
                {t('common.cancel')}
              </button>
            </>
          ) : (
            <button
              type="button"
              className="btn btn-sm"
              disabled={!selected || profileBusy}
              onClick={() => setConfirmDelete(true)}
            >
              {t('settings.images.profileDelete')}
            </button>
          )}
        </div>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <input
            className="input"
            value={profileName}
            placeholder={t('settings.images.profileName')}
            aria-label={t('settings.images.profileName')}
            onChange={(e) => setProfileName(e.target.value)}
          />
          <button
            type="button"
            className="btn btn-sm"
            disabled={!trimmedName || profileBusy}
            onClick={() => void saveProfile()}
          >
            {replacing ? t('settings.images.profileUpdate') : t('settings.images.profileSave')}
          </button>
          {/* Said rather than implied: the credential is not part of a profile, and a user
              who filled the field in below and then stored a profile would otherwise have
              to guess whether it travelled with it. */}
          <span className="faint text-[11px]">{t('settings.images.profileKeyHint')}</span>
        </div>
      </div>

      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="text-[12px]">{t('settings.images.kind')}</span>
        <select
          className="input"
          aria-label={t('settings.images.kind')}
          value={images.kind ?? ''}
          onChange={(e) => {
            const kind = e.target.value
            // The hosted route has no second sampling pass, so a switch left on from a local
            // kind would make every draw fail with "cannot run a second pass" the moment the
            // kind changed. Turning it off with the kind is the one repair the panel can make
            // without hiding a stored setting the user chose.
            update(kind === 'nim' ? { kind, hiresEnabled: false } : { kind })
          }}
        >
          <option value="">{t('settings.images.kindServer')}</option>
          <option value="cli">{t('settings.images.kindCLI')}</option>
          <option value="sdserver">{t('settings.images.kindSD')}</option>
          <option value="nim">{t('settings.images.kindNIM')}</option>
        </select>
        <span className="faint flex-1 text-[11px]">
          {isNIM
            ? t('settings.images.kindNIMHint')
            : isSD
              ? t('settings.images.kindSDHint')
              : isCLI
                ? t('settings.images.kindCLIHint')
                : t('settings.images.kindServerHint')}
        </span>
      </div>

      {/* The pipeline of the configured model set. It is a profile rather than a list of
          flags: steps, guidance, whether a negative prompt means anything, whether a second
          pass was ever measured, and whether the model wants a caption all travel with it.
          Automatic reads the diffusion model's own header, falling back to its file name only
          when that cannot be read, and the resolved name is shown here because a miss is
          otherwise silent — a generic profile captions nothing and draws at the build's
          defaults. Hidden for the hosted kind, whose route takes a prompt, a size and a
          seed and nothing a pipeline would change. */}
      <div className={isNIM ? 'hidden' : 'mb-3 flex flex-wrap items-center gap-2'}>
        <span className="text-[12px]">{t('settings.images.pipeline')}</span>
        <select
          className="input"
          aria-label={t('settings.images.pipeline')}
          value={images.pipeline ?? ''}
          onChange={(e) => update({ pipeline: e.target.value })}
        >
          <option value="">{t('settings.images.pipelineAuto')}</option>
          <option value="flux3-klein">{t('settings.images.pipelineFlux3')}</option>
          <option value="flux3-klein-base">{t('settings.images.pipelineFlux3Base')}</option>
          <option value="generic">{t('settings.images.pipelineGeneric')}</option>
        </select>
        <span className="faint flex-1 text-[11px]">
          {images.pipeline === 'generic'
            ? t('settings.images.pipelineGenericHint')
            : images.pipeline !== ''
              ? t('settings.images.pipelineChosen')
              : resolvedPipeline
                ? // Which evidence resolved it, because the two are different claims: the
                  // tensors are the model's own, the file name is a label somebody chose.
                  t(
                    resolvedPipeline.source === 'weights'
                      ? 'settings.images.pipelineResolvedWeights'
                      : 'settings.images.pipelineResolved',
                    { name: resolvedPipeline.name },
                  )
                : t('settings.images.pipelineHint')}
        </span>
      </div>

      {/* Who writes the caption a caption-trained model is drawn from. Empty is "the
          pipeline decides", which is how a FLUX 3 model gets one without the user finding
          this row; "off" is the explicit no that overrides it.

          Hidden for the hosted kind: captioning is a local pipeline's second model, and
          build.nvidia.com has no such stage. */}
      <div className={isNIM ? 'hidden' : 'mb-3 flex flex-wrap items-center gap-2'}>
        <span className="text-[12px]">{t('settings.images.captioning')}</span>
        <select
          className="input"
          aria-label={t('settings.images.captioning')}
          value={images.captioning ?? ''}
          onChange={(e) => update({ captioning: e.target.value })}
        >
          <option value="">{t('settings.images.captioningPipeline')}</option>
          <option value="off">{t('settings.images.captioningOff')}</option>
          <option value="auto">{t('settings.images.captioningAuto')}</option>
          <option value="model">{t('settings.images.captioningModel')}</option>
        </select>
        <span className="faint flex-1 text-[11px]">
          {images.captioning === 'off'
            ? t('settings.images.captioningOffHint')
            : images.captioning === 'auto'
              ? t('settings.images.captioningAutoHint')
              : images.captioning === 'model'
                ? t('settings.images.captioningModelHint')
                : t('settings.images.captioningPipelineHint')}
        </span>
      </div>

      {/* The process row: what is running right now, and the one control that starts or
          stops it. It is rendered whenever the kind is the managed server *or* a process is
          actually up, because switching the kind away must not hide a loaded model — a
          server with no row is a multi-gigabyte process with nothing left to stop it. */}
      <ImageServerRow
        images={images}
        server={server}
        isSD={isSD}
        busy={serverBusy}
        onUpdate={update}
        onStart={() => void startServer()}
        onStop={() => void stopServer()}
      />

      <div className="grid gap-2 sm:grid-cols-2">
        {isLocal ? (
          <>
            {/* How stable-diffusion.cpp gets here. KiChat does not download it: the
                project publishes one archive per accelerator, so this panel's job is to
                say which one to take for this machine, then to find the program inside
                whatever folder it was unpacked into. */}
            <PathField
              label={t('settings.images.cliFolder')}
              hint={t('settings.images.cliFolderHint', { program })}
              value={images.cliFolder ?? ''}
              placeholder="/home/user/Downloads/sd"
              onChange={(v) => update({ cliFolder: v })}
              onPick={() => pickImageFolder(update, t('settings.images.cliFolder'))}
            />
            {/* Which archive to download, for this machine. The list is the project's own,
                filtered to this platform by the backend: a CUDA archive is published for
                Windows and not for Linux, and a row for it would send a user to a file
                that does not exist. */}
            {builds.length > 0 ? (
              <div className="text-[12px] sm:col-span-2">
                {t('settings.images.cliBuild')}
                <div className="mt-1 flex flex-wrap items-center gap-2">
                  <select
                    className="input"
                    aria-label={t('settings.images.cliBuild')}
                    value={chosenBuild?.id ?? ''}
                    onChange={(e) => setSdBuild(e.target.value)}
                  >
                    {builds.map((b) => (
                      <option key={b.id} value={b.id}>
                        {b.id}
                      </option>
                    ))}
                  </select>
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => {
                      const url = status?.releasePage
                      if (url) void App.OpenExternal(url)
                    }}
                  >
                    {t('settings.images.cliBuildOpen')}
                  </button>
                </div>
                {chosenBuild?.asset ? (
                  <span className="faint mono mt-1 block text-[11px]">{chosenBuild.asset}</span>
                ) : null}
                {chosenBuild?.noteKey ? (
                  <span className="faint mt-1 block text-[11px]">{tDynamic(chosenBuild.noteKey)}</span>
                ) : null}
                {/* What is installed, read off the folder's own libraries — which is a
                    different question from the archive the picker above is pointing at,
                    and the one that decides what a draw actually runs. */}
                {status?.binary ? (
                  <span className="faint mt-1 block text-[11px]">
                    {t('settings.images.cliDetected')}:{' '}
                    {status.build || t('settings.images.cliBuildCpu')}
                  </span>
                ) : null}
              </div>
            ) : (
              <span className="faint text-[11px] sm:col-span-2">
                {t('settings.images.cliBuildNone')}
              </span>
            )}
            {/* The fallback, and drawn as one: while a folder is set this field is
                disabled and says why. Not hidden, because the path is still stored and a
                user who typed it has to be able to see it and clear it. */}
            <PathField
              label={t('settings.images.cliBinary')}
              hint={t('settings.images.cliBinaryHint', { program })}
              value={images.cliBinary ?? ''}
              placeholder={program}
              disabled={folderSet}
              note={folderSet ? t('settings.images.cliBinaryIgnored') : undefined}
              onChange={(v) => update({ cliBinary: v })}
              onPick={() => pickFile(update, t('settings.images.cliBinary'), [], 'cliBinary')}
            />
            {/* What the folder resolved to, so the answer to "isn't that path already
                clear?" is on screen rather than in a hover. Empty while the draft's
                folder differs from the one the status describes. */}
            {foundProgram ? (
              <span className="faint mono text-[11px] sm:col-span-2">
                {t('settings.images.cliFound')}: {foundProgram}
              </span>
            ) : null}
            {isSD && !foundProgram ? (
              /* The one setup the server kind needs and the one-shot kind does not: a folder
                 that holds sd-cli but not sd-server is an *older or partial* unpack, and the
                 sentence names the file to look for rather than silently falling back to
                 the other program. */
              <span className="faint text-[11px] sm:col-span-2">
                {t('settings.images.sdProgramHint', { program })}
              </span>
            ) : null}
            {/* Named one per flag, because the flags are the only documentation there is: a
                user with a working command line can copy the paths across one for one, and
                an empty one is simply not passed. */}
            <PathField
              label={t('settings.images.cliModel')}
              hint={t('settings.images.cliModelHint')}
              value={images.cliModel ?? ''}
              placeholder="v1-5-pruned-emaonly.safetensors"
              onChange={(v) => update({ cliModel: v })}
              onPick={() => pickFile(update, t('settings.images.cliModel'), MODEL_PATTERNS, 'cliModel')}
            />
            <PathField
              label={t('settings.images.cliDiffusionModel')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliDiffusionModel ?? ''}
              placeholder="qwen-image-2.1-Q5_K_M.gguf"
              onChange={(v) => update({ cliDiffusionModel: v })}
              onPick={() =>
                pickFile(update, t('settings.images.cliDiffusionModel'), MODEL_PATTERNS, 'cliDiffusionModel')
              }
            />
            <PathField
              label={t('settings.images.cliVae')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliVae ?? ''}
              placeholder="qwen_image_2.1_vae_bf16.safetensors"
              onChange={(v) => update({ cliVae: v })}
              onPick={() => pickFile(update, t('settings.images.cliVae'), MODEL_PATTERNS, 'cliVae')}
            />
            <PathField
              label={t('settings.images.cliLlm')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliLlm ?? ''}
              placeholder="Qwen3-VL-8B-Instruct-Q5_K_M.gguf"
              onChange={(v) => update({ cliLlm: v })}
              onPick={() => pickFile(update, t('settings.images.cliLlm'), MODEL_PATTERNS, 'cliLlm')}
            />
            <PathField
              label={t('settings.images.cliClipL')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliClipL ?? ''}
              onChange={(v) => update({ cliClipL: v })}
              onPick={() => pickFile(update, t('settings.images.cliClipL'), MODEL_PATTERNS, 'cliClipL')}
            />
            <PathField
              label={t('settings.images.cliClipG')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliClipG ?? ''}
              onChange={(v) => update({ cliClipG: v })}
              onPick={() => pickFile(update, t('settings.images.cliClipG'), MODEL_PATTERNS, 'cliClipG')}
            />
            <PathField
              label={t('settings.images.cliT5xxl')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliT5xxl ?? ''}
              onChange={(v) => update({ cliT5xxl: v })}
              onPick={() => pickFile(update, t('settings.images.cliT5xxl'), MODEL_PATTERNS, 'cliT5xxl')}
            />
            {/* A mode, not a value: Flux on a card too small to hold it is why the switch
                exists, and a user flips it per model rather than per picture. It is sent
                as a bare flag, so nothing here has to agree with a build that spells the
                value differently. */}
            <label
              className="flex items-center gap-2 text-[12px] sm:col-span-2"
              title={t('settings.images.cliOffloadToCpuHint')}
            >
              <input
                type="checkbox"
                checked={images.cliOffloadToCpu ?? false}
                aria-label={t('settings.images.cliOffloadToCpu')}
                onChange={(e) => update({ cliOffloadToCpu: e.target.checked })}
              />
              <span>{t('settings.images.cliOffloadToCpu')}</span>
              <span className="faint flex-1 text-[11px]">
                {t('settings.images.cliOffloadToCpuHint')}
              </span>
            </label>
            {/* Tiling decides how the VAE decodes a picture larger than the card's memory
                can hold at once. It is a bare flag like the offload switch, so it does not
                depend on what the installed build calls the value; a build that lacks the
                flag ignores it rather than failing the draw, and that is stated rather
                than left to be discovered. */}
            <label
              className="flex items-center gap-2 text-[12px] sm:col-span-2"
              title={t('settings.images.cliVaeTilingHint')}
            >
              <input
                type="checkbox"
                checked={images.cliVaeTiling ?? false}
                aria-label={t('settings.images.cliVaeTiling')}
                onChange={(e) => update({ cliVaeTiling: e.target.checked })}
              />
              <span>{t('settings.images.cliVaeTiling')}</span>
              <span className="faint flex-1 text-[11px]">
                {t('settings.images.cliVaeTilingHint')}
              </span>
            </label>
            {images.cliVaeTiling ? (
              <label className="text-[12px] sm:col-span-2">
                {t('settings.images.cliVaeTileSize')}
                <input
                  className="input mono mt-1"
                  type="number"
                  min={64}
                  max={2048}
                  step={64}
                  value={images.cliVaeTileSize || ''}
                  onChange={(e) => update({ cliVaeTileSize: Number(e.target.value) || 0 })}
                  placeholder={t('settings.images.auto')}
                />
              </label>
            ) : null}
            <label className="text-[12px] sm:col-span-2" title={t('settings.images.cliExtraHint')}>
              {t('settings.images.cliExtra')}
              <input
                className="input mono mt-1"
                value={images.cliExtraArgs ?? ''}
                onChange={(e) => update({ cliExtraArgs: e.target.value })}
                placeholder="--diffusion-fa --cfg-scale 1.0 --sigmas 1.0,0.5,0.0"
              />
            </label>
          </>
        ) : isNIM ? (
          <>
            <label className="text-[12px]">
              {t('settings.images.nimModel')}
              <select
                className="input mt-1"
                aria-label={t('settings.images.nimModel')}
                value={images.nimModel || NIM_MODELS[0]}
                onChange={(e) => update({ nimModel: e.target.value })}
              >
                {nimModelOptions(images.nimModel).map((m) => (
                  <option key={m} value={m}>
                    {nimModelUnlisted(m) ? `${m} — ${t('settings.images.nimModelUnlistedTag')}` : m}
                  </option>
                ))}
              </select>
            </label>
            {nimModelUnlisted(images.nimModel) ? (
              <p className="faint text-[11px] sm:col-span-2">{t('settings.images.nimModelUnlisted')}</p>
            ) : null}
            {/* Where the key comes from, said plainly: this section has no field for it, so
                a user who needs to replace it has to be told which tab holds it. */}
            <div
              className="text-[12px]"
              style={{ color: status && !status.keySet ? 'var(--danger)' : 'var(--text-dim)' }}
            >
              {status?.keySet ? t('settings.images.nimKey') : t('settings.images.nimNoKey')}
            </div>
            <p className="faint text-[11px] sm:col-span-2">{t('settings.images.nimWhat')}</p>
            <p className="faint text-[11px] sm:col-span-2">{t('settings.images.nimNoHires')}</p>
          </>
        ) : (
          <>
            <label className="text-[12px]">
              {t('settings.images.baseUrl')}
              <input
                className="input mono mt-1"
                value={images.baseUrl ?? ''}
                onChange={(e) => update({ baseUrl: e.target.value })}
                placeholder="http://127.0.0.1:7860"
              />
            </label>
            <label className="text-[12px]" title={t('settings.images.keyHint')}>
              {t('settings.images.key')}
              <input
                className="input mono mt-1"
                type="password"
                value={images.apiKey ?? ''}
                onChange={(e) => update({ apiKey: e.target.value })}
                placeholder={status?.keySet ? '••••••••' : 'user:pass'}
              />
            </label>
            <label className="text-[12px]">
              {t('settings.images.model')}
              <select
                className="input mt-1"
                value={images.model ?? ''}
                onChange={(e) => update({ model: e.target.value })}
              >
                <option value="">{auto}</option>
                {(status?.models ?? []).map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            </label>
          </>
        )}
        {/* The one sentence that says what this whole group does, where it can be read before
            a field is filled in: empty means the model chooses it per picture, and a typed
            value is fixed for every picture — including the ones the model did not ask for. */}
        <p className={isNIM ? 'hidden' : 'faint text-[11px] sm:col-span-2'}>{t('settings.images.pinHint')}</p>
        <label className={isNIM ? 'hidden' : 'text-[12px]'} title={t('settings.images.pinHint')}>
          {t('settings.images.sampler')}
          <select
            className="input mt-1"
            value={images.sampler ?? ''}
            onChange={(e) => update({ sampler: e.target.value })}
          >
            <option value="">{t('settings.images.modelDecides')}</option>
            {(status?.samplers ?? []).map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
        <label className={isNIM ? 'hidden' : 'text-[12px]'} title={t('settings.images.pinHint')}>
          {t('settings.images.steps')}
          <input
            className="input mt-1"
            type="number"
            min={0}
            max={150}
            value={images.steps || ''}
            onChange={(e) => update({ steps: Number(e.target.value) || 0 })}
            placeholder={t('settings.images.modelDecides')}
          />
        </label>
        <label className={isNIM ? 'hidden' : 'text-[12px]'} title={t('settings.images.cfgHint')}>
          {t('settings.images.cfgScale')}
          <input
            className="input mt-1"
            type="number"
            min={1}
            max={30}
            step={0.5}
            value={images.cfgScale || ''}
            onChange={(e) => update({ cfgScale: Number(e.target.value) || 0 })}
            placeholder={t('settings.images.modelDecides')}
          />
        </label>
        <label className="text-[12px]" title={t('settings.images.pinHint')}>
          {t('settings.images.size')}
          <div className="mt-1 flex items-center gap-2">
            <input
              className="input mono"
              type="number"
              min={0}
              max={2048}
              step={8}
              value={images.width || ''}
              onChange={(e) => update({ width: Number(e.target.value) || 0 })}
              placeholder="512"
            />
            <span className="faint">×</span>
            <input
              className="input mono"
              type="number"
              min={0}
              max={2048}
              step={8}
              value={images.height || ''}
              onChange={(e) => update({ height: Number(e.target.value) || 0 })}
              placeholder="512"
            />
          </div>
          {/* The trained-at squares and the two 16:9 bases, one click each. Setting both
              numbers is the whole of what a preset does — plus, for the 16:9 pair, the
              enlargement that lands on the size its label names. A size this row does
              not offer is still typed into the boxes above. */}
          <div className="mt-1 flex flex-wrap items-center gap-1">
            {SIZE_PRESETS.map((preset) => {
              const label = preset.labelKey ? tDynamic(preset.labelKey) : `${preset.width}²`
              const title = preset.hintKey
                ? tDynamic(preset.hintKey)
                : `${t('settings.images.sizePreset')} ${preset.width}×${preset.height}`
              return (
                <button
                  key={`${preset.width}x${preset.height}`}
                  type="button"
                  className="btn btn-sm"
                  aria-label={title}
                  title={title}
                  onClick={() =>
                    update(
                      preset.scale
                        ? { width: preset.width, height: preset.height, hiresScale: preset.scale }
                        : { width: preset.width, height: preset.height },
                    )
                  }
                >
                  {label}
                </button>
              )
            })}
          </div>
        </label>
        <label className={isNIM ? 'hidden' : 'text-[12px] sm:col-span-2'} title={t('settings.images.pinHint')}>
          {t('settings.images.negative')}
          <input
            className="input mt-1"
            value={images.negativePrompt ?? ''}
            onChange={(e) => update({ negativePrompt: e.target.value })}
            placeholder={t('settings.images.modelDecides')}
          />
        </label>
        {/* The second sampling pass. It is a fixed setting rather than something the model
            may ask for, because it doubles the time a draw costs and turns a medium
            picture into a large one — a decision for the person paying for the GPU. Its
            two numbers stay stored while the switch is off, so flipping it back restores
            what was chosen instead of resetting it. */}
        <div className={isNIM ? 'hidden' : 'sm:col-span-2'}>
          <label className="flex items-center gap-2 text-[12px]" title={t('settings.images.hiresHint')}>
            <input
              type="checkbox"
              checked={images.hiresEnabled ?? false}
              aria-label={t('settings.images.hires')}
              onChange={(e) => update({ hiresEnabled: e.target.checked })}
            />
            <span>{t('settings.images.hires')}</span>
            <span className="faint flex-1 text-[11px]">{t('settings.images.hiresHint')}</span>
          </label>
          {images.hiresEnabled ? (
            <div className="mt-2 grid gap-2 sm:grid-cols-2">
              <label className="text-[12px]">
                {t('settings.images.hiresScale')}
                <input
                  className="input mono mt-1"
                  type="number"
                  min={IMAGE_HIRES_LIMITS.minScale}
                  max={IMAGE_HIRES_LIMITS.maxScale}
                  step={0.05}
                  value={images.hiresScale || ''}
                  onChange={(e) => update({ hiresScale: Number(e.target.value) || 0 })}
                  placeholder={String(IMAGE_HIRES_LIMITS.defaultScale)}
                />
              </label>
              <label className="text-[12px]" title={t('settings.images.hiresDenoiseHint')}>
                {t('settings.images.hiresDenoise')}
                <input
                  className="input mono mt-1"
                  type="number"
                  min={IMAGE_HIRES_LIMITS.minDenoise}
                  max={IMAGE_HIRES_LIMITS.maxDenoise}
                  step={0.05}
                  value={images.hiresDenoise || ''}
                  onChange={(e) => update({ hiresDenoise: Number(e.target.value) || 0 })}
                  placeholder={String(IMAGE_HIRES_LIMITS.defaultDenoise)}
                />
              </label>
            </div>
          ) : null}
        </div>
        {/* Enlarging. It is deliberately not gated on `enabled`: resampling an attached
            picture needs no image server at all, and a user who only wants that should not
            have to switch on a diffusion backend to get it. */}
        <div className="mt-1 border-t pt-2 sm:col-span-2" style={{ borderColor: 'var(--border)' }}>
          <div className="text-[12px] font-medium">{t('settings.images.upscaling')}</div>
          <label className="mt-1 flex items-center gap-2 text-[12px]">
            <input
              type="checkbox"
              checked={images.upscalerEnabled ?? false}
              aria-label={t('settings.images.upscalerEnabled')}
              onChange={(e) => update({ upscalerEnabled: e.target.checked })}
            />
            <span>{t('settings.images.upscalerEnabled')}</span>
            <span className="faint flex-1 text-[11px]">{t('settings.images.upscalerHint')}</span>
          </label>
          <div className="mt-2 grid gap-2 sm:grid-cols-2">
            <label className="text-[12px]">
              {t('settings.images.upscaleDefaultMethod')}
              <select
                className="input mt-1"
                value={images.upscaleDefaultMethod || 'lanczos'}
                onChange={(e) => update({ upscaleDefaultMethod: e.target.value })}
              >
                <option value="lanczos">{t('settings.images.upscaleResample')}</option>
                <option value="esrgan" disabled={!images.upscalerEnabled}>
                  {t('settings.images.upscaleNeural')}
                </option>
              </select>
            </label>
            <label className="text-[12px]">
              {t('settings.images.upscaleSharpen')}
              <input
                className="input mono mt-1"
                type="number"
                min={0}
                max={1}
                step={0.05}
                value={images.upscaleSharpen ?? ''}
                onChange={(e) => update({ upscaleSharpen: Number(e.target.value) || 0 })}
                placeholder="0.25"
              />
            </label>
          </div>
          {/* The download the app can do itself. It is deliberately outside the block the
              switch gates: a user who has not switched the neural method on yet is exactly
              the user who needs the program, and the switch then appears as a consequence of
              installing rather than as a condition of it. A platform with no archive is told
              so here, where the button would have been, instead of after a failed fetch. */}
          {upscalerInstall?.unsupported ? (
            <p className="faint mt-2 text-[11px]">
              {t('settings.images.upscalerNoArchive', { platform: upscalerInstall.unsupported })}
            </p>
          ) : (
            <div className="mt-2 flex flex-wrap items-center gap-2">
              <button
                type="button"
                className="btn btn-sm"
                disabled={installing}
                onClick={() => void downloadUpscaler()}
              >
                {t('settings.images.upscalerInstall')}
              </button>
              {installing ? (
                <span className="text-[11px] faint">
                  {upscalerInstall?.phase === 'extracting'
                    ? t('settings.images.upscalerExtracting')
                    : upscalerInstall?.phase === 'verifying'
                      ? t('settings.images.upscalerVerifying')
                      : t('settings.images.upscalerInstalling')}
                  {upscalerInstall?.phase === 'downloading' && upscalerInstall.total > 0
                    ? ` ${Math.min(100, Math.round((upscalerInstall.done / upscalerInstall.total) * 100))}%`
                    : '…'}
                </span>
              ) : (
                <span className="faint flex-1 text-[11px]">
                  {t('settings.images.upscalerInstallHint', {
                    size: formatBytes(upscalerInstall?.size ?? 0),
                  })}
                </span>
              )}
            </div>
          )}
          {images.upscalerEnabled ? (
            <div className="mt-2 grid gap-2 sm:grid-cols-2">
              <label className="text-[12px] sm:col-span-2">
                {t('settings.images.upscalerBinary')}
                <div className="mt-1 flex items-center gap-2">
                  <input
                    className="input mono flex-1"
                    value={images.esrganBinary ?? ''}
                    onChange={(e) => update({ esrganBinary: e.target.value })}
                    placeholder={t('settings.images.upscalerPathHint')}
                  />
                  <button type="button" className="btn btn-sm" onClick={() => void pickUpscaler()}>
                    {t('settings.images.upscalerPick')}
                  </button>
                </div>
              </label>
              <label className="text-[12px]">
                {t('settings.images.upscalerModel')}
                <select
                  className="input mt-1"
                  value={images.esrganModel || 'realesrgan-x4plus'}
                  onChange={(e) => update({ esrganModel: e.target.value })}
                >
                  {(upscaler?.models?.length
                    ? upscaler.models
                    : ['realesrgan-x4plus', 'realesrgan-x4plus-anime']
                  ).map((name) => (
                    <option key={name} value={name}>
                      {name}
                    </option>
                  ))}
                </select>
              </label>
              <label className="text-[12px]" title={t('settings.images.upscalerTileHint')}>
                {t('settings.images.upscalerTile')}
                <input
                  className="input mono mt-1"
                  type="number"
                  min={0}
                  max={4096}
                  step={64}
                  value={images.esrganTile || ''}
                  onChange={(e) => update({ esrganTile: Number(e.target.value) || 0 })}
                  placeholder="0"
                />
              </label>
              <div className="flex flex-wrap items-center gap-2 sm:col-span-2">
                <button
                  type="button"
                  className="btn btn-sm"
                  disabled={upscalerBusy}
                  onClick={() => void testUpscaler()}
                >
                  {upscalerBusy ? t('settings.images.testing') : t('settings.images.upscalerTest')}
                </button>
                <span className="faint text-[11px]">
                  {upscaler?.available
                    ? `${t('settings.images.upscalerReady')}: ${upscaler.path}`
                    : upscaler?.error
                      ? errText(upscaler.error)
                      : t('settings.images.upscalerNotReady')}
                </span>
              </div>
            </div>
          ) : null}
        </div>
        <label className="text-[12px] sm:col-span-2">
          {t('settings.images.saveFolder')}
          <div className="mt-1 flex items-center gap-2">
            <input
              className="input mono flex-1"
              value={images.saveFolder ?? ''}
              onChange={(e) => update({ saveFolder: e.target.value })}
            />
            <button
              type="button"
              className="btn btn-sm"
              onClick={async () => {
                try {
                  const dir = await App.PickSaveFolder()
                  if (dir) update({ saveFolder: dir })
                } catch (err) {
                  useStore.getState().toast('error', errText(err))
                }
              }}
            >
              {t('settings.images.pickFolder')}
            </button>
          </div>
        </label>
      </div>

      <div className="mt-3 flex items-center gap-2">
        <button
          type="button"
          className="btn btn-sm"
          disabled={testing || !images.enabled || (isSD && !sdRunning) || (isNIM && !status?.keySet)}
          onClick={() => void test()}
        >
          {testing ? t('settings.images.testing') : t('settings.images.test')}
        </button>
        {/* The test is a real draw, and on the one-shot kind that is a model load before
            anything is drawn — so a Stop belongs beside it rather than only under a
            picture. It appears only while the test runs: at rest there is nothing to
            stop, and a permanently visible button would read as a second action. */}
        {testing ? (
          <button
            type="button"
            className="btn btn-sm btn-danger"
            title={t('images.stopHint')}
            onClick={() => void stopTest()}
          >
            ⏹ {t('images.stop')}
          </button>
        ) : null}
        {/* What the test actually costs, which is different for all three kinds: a WebUI
            answers in seconds, the one-shot program loads the model from disk first (minutes
            on a CPU), and the managed server is already loaded — unless it is stopped, in
            which case the test is disabled and the Start button next to it is the action. */}
        <span className="faint text-[11px]">
          {isNIM
            ? t('settings.images.nimTestHint')
            : isSD
              ? sdRunning
                ? t('settings.images.sdTestHint')
                : t('settings.images.sdStartFirst')
              : isCLI
                ? t('settings.images.cliDefaults')
                : t('settings.images.defaults')}
        </span>
      </div>

      {images.enabled && status?.error && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--danger)' }}>
          {errText(status.error)}
        </div>
      )}
      {images.enabled && !isCLI && status?.ok && status.models.length === 0 && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--warn)' }}>
          {t('settings.images.noModels')}
        </div>
      )}

      <PictureStore />
    </Section>
  )
}

/**
 * The fields a start of the managed server reads: the half of its command line that describes
 * the load.
 *
 * It mirrors `images.CLI.ServerKey` on the backend — the folder, the program, the model set,
 * the offload switch, the user's own arguments and the VAE decode switches — and nothing else.
 * The listener is deliberately not part of it: a port is chosen when the process starts, so
 * including one would make every running server look out of date the moment it came up. This is
 * the only place the frontend knows which fields those are, and it knows them for one question:
 * will applying this profile need a reload?
 */
function startupKey(s: config.ImageSettings): string {
  return JSON.stringify([
    s.kind,
    s.cliFolder,
    s.cliBinary,
    s.cliModel,
    s.cliDiffusionModel,
    s.cliVae,
    s.cliLlm,
    s.cliClipL,
    s.cliClipG,
    s.cliT5xxl,
    s.cliOffloadToCpu,
    s.cliExtraArgs,
    // The VAE decode is part of the load rather than of a request, so turning tiling on —
    // or changing the tile under it — is the same kind of change as a different checkpoint.
    s.cliVaeTiling,
    s.cliVaeTileSize,
  ])
}

/**
 * The managed picture server's process row.
 *
 * It is rendered whenever the kind is the managed server, or when a process is really up —
 * including the state a user reaches by switching the kind away while a model is loaded. That
 * second case is the reason this is not simply part of the server half of the form: a running
 * server with no row is a multi-gigabyte process with nothing left in the window that stops it,
 * which is exactly the state the chat backends' own model switch refuses to leave behind.
 */
function ImageServerRow({
  images,
  server,
  isSD,
  busy,
  onUpdate,
  onStart,
  onStop,
}: {
  images: config.ImageSettings
  server?: main.ImageServerStatus
  isSD: boolean
  busy: boolean
  onUpdate: (changes: Partial<config.ImageSettings>) => void
  onStart: () => void
  onStop: () => void
}) {
  const t = useStore((s) => s.t)
  // Keys built from a value the backend reports, rather than written out here.
  const tDynamic = useStore((s) => s.tDynamic)
  const state = server?.state || 'stopped'
  const running = state === 'running' || state === 'starting'
  if (!isSD && !running && state !== 'failed') return null

  return (
    <div className="mb-3 border-t pt-3" style={{ borderColor: 'var(--border)' }}>
      <div className="mb-1 text-[12px]">{t('settings.images.sdTitle')}</div>
      <p className="faint mb-2 text-[11px]">{t('settings.images.sdHint')}</p>
      <div className="flex flex-wrap items-center gap-2">
        {/* Start and Restart belong to the kind this section selects. A server left running
            under another backend choice is a process the row can only *stop* — starting it
            again is not something this form can ask for, because the command line a start
            builds comes from the settings the other kind replaced. */}
        {isSD ? (
          <button
            type="button"
            className="btn btn-sm"
            disabled={busy}
            title={t('settings.images.sdStartHint')}
            onClick={onStart}
          >
            {state === 'stopped' || state === 'failed'
              ? t('settings.managed.start')
              : t('settings.images.sdRestart')}
          </button>
        ) : null}
        <button type="button" className="btn btn-sm" disabled={busy || state === 'stopped'} onClick={onStop}>
          {t('settings.managed.stop')}
        </button>
        <span className={`badge ${state === 'running' ? 'badge-ok' : state === 'failed' ? 'badge-danger' : ''}`}>
          {tDynamic(`settings.managed.state.${state}`)}
        </span>
        {/* The address the process settled on, so a user who wants to look at the server's own
            page, or point another tool at it, does not have to guess which port was free. */}
        {server?.url ? <span className="mono min-w-0 flex-1 truncate text-[11px] faint">{server.url}</span> : null}
        {server?.restartNeeded && isSD ? (
          <span className="badge badge-warn" title={t('settings.images.sdRestartNeededHint')}>
            {t('settings.images.sdRestartNeeded')}
          </span>
        ) : null}
      </div>

      {isSD ? (
        <label className="mt-2 flex items-center gap-2 text-[11px]" title={t('settings.images.sdStartWithAppHint')}>
          <input
            type="checkbox"
            checked={images.startWithApp ?? false}
            aria-label={t('settings.images.sdStartWithApp')}
            onChange={(e) => onUpdate({ startWithApp: e.target.checked })}
          />
          <span>{t('settings.images.sdStartWithApp')}</span>
          <span className="faint flex-1">{t('settings.images.sdStartWithAppHint')}</span>
        </label>
      ) : null}

      {/* Said where it can be acted on rather than left to be discovered: with the server down
          the model has no generate_image tool at all, so a user who asks for a picture would
          be told by the model that it cannot draw. */}
      {isSD && !running && state !== 'failed' ? (
        <p className="faint mt-1 text-[11px]">{t('settings.images.sdNoTool')}</p>
      ) : null}
      {!isSD && running ? (
        <p className="faint mt-1 text-[11px]">{t('settings.images.sdOtherKind')}</p>
      ) : null}
      {server?.error ? (
        <div className="mt-2 whitespace-pre-wrap text-[11px]" style={{ color: 'var(--danger)' }}>
          {errText(server.error)}
        </div>
      ) : null}
      {state === 'failed' && server?.log?.length ? (
        <pre className="faint mono mt-2 max-h-32 overflow-y-auto scroll-thin whitespace-pre-wrap text-[10px]">
          {server.log.join('\n')}
        </pre>
      ) : null}
    </div>
  )
}

/**
 * The pictures KiChat is holding, and the one control that frees them.
 *
 * A drawn picture is an attachment like any other, which means it lives in the app's own
 * database — that is where an unsaved picture "is", and it is what this row answers. The
 * store keeps them for as long as the chat exists, and nothing ever removes them: a long
 * session of drawing quietly grows the data directory, and a user who never presses Save
 * has no way to say they are finished with a picture.
 *
 * The removal is deliberately narrow. It touches neither a picture its owner has a file of
 * (the export note, written by SaveAttachmentAs) nor a picture the *user* attached — the
 * backend tells those apart by the recipe, which only a drawn picture has, so a photo dropped
 * into the composer can never be swept up by a cache button.
 *
 * It also asks twice. "Clear cache" is the kind of label that sounds like it costs a
 * re-download, and what it actually costs is a picture the chat will never show again — so
 * the second press names the count, the size and how many conversations lose something.
 */
function PictureStore() {
  const t = useStore((s) => s.t)
  const [cache, setCache] = useState<main.ImageCache | null>(null)
  const [busy, setBusy] = useState(false)
  const [confirming, setConfirming] = useState(false)

  const refresh = async () => {
    try {
      setCache(await App.GetImageCache())
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    }
  }

  // Read once when the panel opens, like the backend probe above: the count is what the
  // store holds right now, and re-reading it on every keystroke in an unrelated field would
  // be a database walk per character.
  useEffect(() => {
    void refresh()
  }, [])

  const clear = async () => {
    setBusy(true)
    try {
      const result = await App.ClearImageCache()
      useStore
        .getState()
        .toast('info', t('settings.images.cacheCleared', { count: result.removed, bytes: formatBytes(result.bytes) }))
    } catch (err) {
      useStore.getState().toast('error', `${t('settings.images.cacheFailed')}: ${errText(err)}`)
    } finally {
      setBusy(false)
      setConfirming(false)
      await refresh()
    }
  }

  // Read as an object with numbers defaulted rather than trusted: this renders on a fresh
  // install, where every field is zero, and a missing one must not print "undefined".
  const count = cache?.count ?? 0
  const bytes = cache?.bytes ?? 0
  const unsavedCount = cache?.unsavedCount ?? 0
  const unsavedBytes = cache?.unsavedBytes ?? 0
  const unsavedChats = cache?.unsavedChats ?? 0

  return (
    <div className="mt-4 border-t pt-3" style={{ borderColor: 'var(--border)' }}>
      <div className="mb-1 text-[12px]">{t('settings.images.cache')}</div>
      <p className="faint mb-2 text-[11px]">{t('settings.images.cacheHint')}</p>

      {count === 0 ? (
        <div className="faint text-[11px]">{t('settings.images.cacheEmpty')}</div>
      ) : (
        <>
          <div className="text-[12px]">
            {t('settings.images.cacheCount', { count: formatNumber(count), bytes: formatBytes(bytes) })}
          </div>
          {unsavedCount > 0 ? (
            <div className="text-[12px]" style={{ color: 'var(--warn)' }}>
              {t('settings.images.cacheUnsaved', {
                count: formatNumber(unsavedCount),
                bytes: formatBytes(unsavedBytes),
                chats: formatNumber(unsavedChats),
              })}
            </div>
          ) : (
            <div className="faint text-[12px]">{t('settings.images.cacheAllSaved')}</div>
          )}
        </>
      )}

      {/* The honest limit of the reading, said before the button rather than after the
          loss: this app only knows about a copy it was told about. */}
      <p className="faint mt-1 text-[11px]">{t('settings.images.cacheOlder')}</p>

      <div className="mt-2 flex flex-wrap items-center gap-2">
        {confirming ? (
          <>
            <span className="text-[11px]" style={{ color: 'var(--danger)' }}>
              {t('settings.images.cacheConfirm', {
                count: formatNumber(unsavedCount),
                bytes: formatBytes(unsavedBytes),
                chats: formatNumber(unsavedChats),
              })}
            </span>
            <button
              type="button"
              className="btn btn-sm btn-danger"
              disabled={busy}
              onClick={() => void clear()}
            >
              {t('settings.images.cacheConfirmYes', { count: formatNumber(unsavedCount) })}
            </button>
            <button type="button" className="btn btn-sm" disabled={busy} onClick={() => setConfirming(false)}>
              {t('settings.images.cacheCancel')}
            </button>
          </>
        ) : (
          <button
            type="button"
            className="btn btn-sm"
            disabled={busy || unsavedCount === 0}
            title={t('settings.images.cacheClearHint')}
            onClick={() => setConfirming(true)}
          >
            {t('settings.images.cacheClear')}
          </button>
        )}
      </div>
    </div>
  )
}

