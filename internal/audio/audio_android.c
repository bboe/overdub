// OpenSL ES, which is the layer AudioFlinger mixes. That is what lets the
// chime coexist with Alexa: her speech is a track like ours, not an owner of
// the device. Writing to ALSA directly reaches the speaker and wedges the
// driver, which docs/audio.md records.

#include "audio.h"

#include <SLES/OpenSLES.h>
#include <SLES/OpenSLES_Android.h>

#define CHUNK 16384
#define NUM_BUFFERS 8

static const unsigned char *clip;
static size_t clip_len;

static SLObjectItf engine_obj, mix_obj, player_obj;
static SLPlayItf player_play;
static SLAndroidSimpleBufferQueueItf player_queue;

#define TRY(expr)                                   \
	do {                                            \
		if ((expr) != SL_RESULT_SUCCESS) return -1; \
	} while (0)

// Init unwinds what it built. Play does not: a failed enqueue should cost one
// chime rather than every chime after it.
#define TRY_INIT(expr)                   \
	do {                                 \
		if ((expr) != SL_RESULT_SUCCESS) { \
			audio_close();               \
			return -1;                   \
		}                                \
	} while (0)

static SLuint32 milli_hz(int rate) {
	switch (rate) {
	case 8000: return SL_SAMPLINGRATE_8;
	case 16000: return SL_SAMPLINGRATE_16;
	case 22050: return SL_SAMPLINGRATE_22_05;
	case 24000: return SL_SAMPLINGRATE_24;
	case 32000: return SL_SAMPLINGRATE_32;
	case 44100: return SL_SAMPLINGRATE_44_1;
	case 48000: return SL_SAMPLINGRATE_48;
	default: return 0;
	}
}

int audio_init(const unsigned char *pcm, size_t len, int rate, int channels) {
	SLuint32 sl_rate = milli_hz(rate);
	if (sl_rate == 0 || channels < 1 || channels > 2) return -1;

	int frame_bytes = channels * 2;
	size_t usable = len - (len % (size_t)frame_bytes);
	if (usable == 0) return -1;

	// The whole clip is queued at once rather than streamed by a callback, so
	// it has to fit. Checked here rather than truncated there: a chime that
	// outgrew the queue would otherwise play its first half for ever.
	if (usable > (size_t)CHUNK * NUM_BUFFERS) return -1;

	clip = pcm;
	clip_len = usable;

	TRY_INIT(slCreateEngine(&engine_obj, 0, NULL, 0, NULL, NULL));
	TRY_INIT((*engine_obj)->Realize(engine_obj, SL_BOOLEAN_FALSE));
	SLEngineItf engine;
	TRY_INIT((*engine_obj)->GetInterface(engine_obj, SL_IID_ENGINE, &engine));
	TRY_INIT((*engine)->CreateOutputMix(engine, &mix_obj, 0, NULL, NULL));
	TRY_INIT((*mix_obj)->Realize(mix_obj, SL_BOOLEAN_FALSE));

	SLDataLocator_AndroidSimpleBufferQueue loc = {
	    SL_DATALOCATOR_ANDROIDSIMPLEBUFFERQUEUE, NUM_BUFFERS};
	SLDataFormat_PCM fmt = {
	    SL_DATAFORMAT_PCM, (SLuint32)channels, sl_rate,
	    SL_PCMSAMPLEFORMAT_FIXED_16, SL_PCMSAMPLEFORMAT_FIXED_16,
	    channels == 1 ? SL_SPEAKER_FRONT_CENTER
	                  : (SL_SPEAKER_FRONT_LEFT | SL_SPEAKER_FRONT_RIGHT),
	    SL_BYTEORDER_LITTLEENDIAN};
	SLDataSource src = {&loc, &fmt};
	SLDataLocator_OutputMix omix = {SL_DATALOCATOR_OUTPUTMIX, mix_obj};
	SLDataSink sink = {&omix, NULL};

	// The configuration interface is asked for but not required, and the get
	// below is guarded to match: it only sets the stream type, so a ROM without
	// it should still chime rather than leave the daemon permanently silent.
	const SLInterfaceID ids[] = {SL_IID_BUFFERQUEUE, SL_IID_ANDROIDCONFIGURATION};
	const SLboolean req[] = {SL_BOOLEAN_TRUE, SL_BOOLEAN_FALSE};
	TRY_INIT((*engine)->CreateAudioPlayer(engine, &player_obj, &src, &sink, 2, ids, req));

	SLAndroidConfigurationItf config;
	if ((*player_obj)->GetInterface(player_obj, SL_IID_ANDROIDCONFIGURATION, &config)
	    == SL_RESULT_SUCCESS) {
		SLint32 stream = SL_ANDROID_STREAM_MEDIA;
		(*config)->SetConfiguration(config, SL_ANDROID_KEY_STREAM_TYPE, &stream,
		                            sizeof(stream));
	}

	TRY_INIT((*player_obj)->Realize(player_obj, SL_BOOLEAN_FALSE));
	TRY_INIT((*player_obj)->GetInterface(player_obj, SL_IID_PLAY, &player_play));
	TRY_INIT((*player_obj)->GetInterface(player_obj, SL_IID_BUFFERQUEUE, &player_queue));
	return 0;
}

// Stopped and cleared first, so a press during the previous chime restarts it
// rather than queueing a second copy behind it. No callback refills this: the
// clip fits the queue, so every buffer it will need is enqueued here, on this
// thread, and there is no in-flight refill for the clear to race.
int audio_play(void) {
	if (player_play == NULL) return -1;
	TRY((*player_play)->SetPlayState(player_play, SL_PLAYSTATE_STOPPED));
	TRY((*player_queue)->Clear(player_queue));
	for (size_t off = 0; off < clip_len; off += CHUNK) {
		size_t n = clip_len - off;
		if (n > CHUNK) n = CHUNK;
		TRY((*player_queue)->Enqueue(player_queue, clip + off, n));
	}
	TRY((*player_play)->SetPlayState(player_play, SL_PLAYSTATE_PLAYING));
	return 0;
}

void audio_close(void) {
	if (player_play != NULL) {
		(*player_play)->SetPlayState(player_play, SL_PLAYSTATE_STOPPED);
		player_play = NULL;
	}
	player_queue = NULL;
	if (player_obj != NULL) {
		(*player_obj)->Destroy(player_obj);
		player_obj = NULL;
	}
	if (mix_obj != NULL) {
		(*mix_obj)->Destroy(mix_obj);
		mix_obj = NULL;
	}
	if (engine_obj != NULL) {
		(*engine_obj)->Destroy(engine_obj);
		engine_obj = NULL;
	}
	// Dropped last: the queue held a pointer into it, and the caller frees it
	// once this returns.
	clip = NULL;
	clip_len = 0;
}
