import apis, { Isu, GraphRequest } from '/@/lib/apis'
import { useCallback } from 'react'
import NowLoading from '/@/components/UI/NowLoading'
import TransitionGraph from './TransitionGraph'
import SittingGraph from './SittingGraph'
import useGraph from './use/graph'
import GraphNavigator from './GraphNavigator'
import { useState } from 'react'
import NowLoadingOverlay from '/@/components/UI/NowLoadingOverlay'

interface Props {
  isu: Isu
}

const IsuGraphCardContent = ({ isu }: Props) => {
  const [isLoading, setIsLoading] = useState(false)
  const getGraphs = useCallback(
    async (params: GraphRequest) => {
      setIsLoading(true)
      const res = await apis.getIsuGraphs(isu.jia_isu_uuid, params)
      setIsLoading(false)
      return res
    },
    [isu.jia_isu_uuid]
  )

  const {
    loading,
    transitionData,
    sittingData,
    timeCategories,
    day,
    conditions,
    specify,
    prev,
    next
  } = useGraph(getGraphs)

  if (loading) return <NowLoading />

  return (
    <div className="flex flex-col gap-12">
      <div className="flex justify-center w-full">
        <GraphNavigator prev={prev} next={next} specify={specify} day={day} />
      </div>
      <div className="relative flex flex-col gap-8">
        <div className="z-10">
          <TransitionGraph
            transitionData={transitionData}
            timeCategories={timeCategories}
            tooltipData={conditions}
          />
        </div>

        <div className="absolute top-0 w-full">
          <SittingGraph
            sittingData={sittingData}
            timeCategories={timeCategories}
          />
        </div>
        {isLoading ? <NowLoadingOverlay /> : null}
      </div>
    </div>
  )
}

export default IsuGraphCardContent
